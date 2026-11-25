// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// RuleSetVersion names the labelling rules below. Labels are recomputed from
// the unchanged stored turns and evidence; only this version changes them.
const RuleSetVersion = "struggle-v1.0"

// Label is the proposed 0/1 label of one turn and why. Users can add context
// to a submission but cannot change a label.
type Label struct {
	TurnID      string   `json:"turn_id"`
	Value       int      `json:"value"` // 1 productive struggle, 0 not shown
	Reason      string   `json:"reason"`
	Signals     []string `json:"signals"`
	Evidence    []string `json:"evidence"` // item IDs supporting the label
	Coverage    string   `json:"coverage"` // full, partial, none
	NeedsReview bool     `json:"needs_review"`
	RuleSet     string   `json:"rule_set"`
	// Human interval this turn can credit, before deduplication.
	IntervalStart int64 `json:"interval_start"`
	IntervalEnd   int64 `json:"interval_end"`
	// Seconds credited after removing overlap with other turns.
	ProductiveSeconds int64  `json:"productive_seconds"`
	Coaching          string `json:"coaching,omitempty"`
}

var (
	diagnosisRe  = regexp.MustCompile(`(?i)\b(why|because|error|exception|traceback|fails?|failing|failed|bug|broken|expected|instead|actually|hypothes[ie]s|suspect|maybe|i think|i tried|tried|root cause|stack ?trace|reproduc\w*|doesn'?t|isn'?t|wrong)\b`)
	constraintRe = regexp.MustCompile(`(?i)\b(must|should(n'?t)?|don'?t|do not|without|only|instead of|keep|avoid|never|make sure|but not|rather than)\b`)
	reviewRe     = regexp.MustCompile(`(?i)\b(review|explain|walk me through|what does|how does|why did|check|verify|compare|trade-?off|alternative|is this right|are you sure)\b`)
	specificRe   = regexp.MustCompile("(`[^`]+`|\\b[\\w./-]+\\.(go|rb|py|js|ts|svelte|rs|c|cpp|h|java|kt|swift|json|ya?ml|toml|md|html|css|sql)\\b|\\b[a-z]+[A-Z]\\w*\\b|\\b\\w+_\\w+\\b|\\b\\w+\\(\\)|line \\d+|:\\d+\\b)")
	testCmdRe    = regexp.MustCompile(`(?i)\b(test|tests|pytest|unittest|jest|vitest|mocha|rspec|rails test|go test|cargo test|npm (run )?test|bun test|make check|ctest|phpunit)\b`)
	slotRe       = regexp.MustCompile(`(?i)^\s*(try again|again|retry|redo|continue|go on|keep going|do it|fix it|fix|still (broken|not working)|same|no|nope|that didn'?t work|it doesn'?t work|doesn'?t work|ok|okay|yes|y|k|next|more|please|pls)[\s.!?]*$`)
)

func words(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		if len(w) > 2 {
			out[w] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// labelTurns labels the given turns, oldest first, and deduplicates their
// human intervals so overlapping turns, sessions and parallel agents never
// credit the same second twice.
func (s *Store) labelTurns(turns []Turn) ([]Label, map[string][]StoredItem, error) {
	items := map[string][]StoredItem{}
	labels := make([]Label, 0, len(turns))
	for i, t := range turns {
		its, err := s.itemsForTurn(t.ID)
		if err != nil {
			return nil, nil, err
		}
		items[t.ID] = its
		earlier, err := s.earlierPrompts(t)
		if err != nil {
			return nil, nil, err
		}
		prev, err := s.previousTurn(t)
		if err != nil {
			return nil, nil, err
		}
		var next *Turn
		for j := i + 1; j < len(turns); j++ {
			if turns[j].Session == t.Session && turns[j].Harness == t.Harness {
				next = &turns[j]
				break
			}
		}
		labels = append(labels, labelTurn(t, its, earlier, prev, next))
	}
	dedupe(labels)
	return labels, items, nil
}

// labelTurn applies the rule set to one turn. It looks for evidence of
// diagnosis, new constraints, the person's own edits, review and
// verification. Prompt length, attempt count, a passing test or a
// surviving diff are never enough on their own for a 1.
func labelTurn(t Turn, its []StoredItem, earlier []StoredItem, prev, next *Turn) Label {
	l := Label{TurnID: t.ID, RuleSet: RuleSetVersion, Coverage: "full"}
	var prompt string
	var promptID string
	var purged, tests, failingTests, passingTests int
	var humanEdit, agentDiff bool
	// Claude Code tool results carry no command; join them to their call.
	commands := map[string]string{}
	for _, it := range its {
		if it.Item.Kind == "tool_call" && it.Item.ToolID != "" {
			commands[it.Item.ToolID] = it.Item.Command
		}
	}
	for _, it := range its {
		if it.Item.Command == "" && it.Item.ToolID != "" {
			it.Item.Command = commands[it.Item.ToolID]
		}
		if it.Purged {
			purged++
			continue
		}
		switch it.Item.Kind {
		case "prompt":
			prompt += it.Item.Text + "\n"
			promptID = it.ID
		case "between_turns_change":
			humanEdit = true
			l.Evidence = append(l.Evidence, it.ID)
		case "diff":
			agentDiff = true
		case "tool_output", "tool_call":
			if testCmdRe.MatchString(it.Item.Command) {
				if it.Item.Kind == "tool_output" {
					tests++
					if it.Item.IsError {
						failingTests++
					} else {
						passingTests++
					}
					l.Evidence = append(l.Evidence, it.ID)
				}
			}
		}
	}
	if promptID != "" {
		l.Evidence = append([]string{promptID}, l.Evidence...)
	}

	l.IntervalStart, l.IntervalEnd = humanInterval(t, next)

	if t.Harness == standaloneHarness {
		return labelStandalone(t, its, l)
	}
	switch {
	case t.PromptLength == 0:
		l.Value, l.Coverage = 0, "none"
		l.IntervalEnd = l.IntervalStart
		l.Reason = "Agent activity with no human prompt: counted as agent time only."
		l.Signals = append(l.Signals, "no_prompt")
		return l
	case prompt == "" && purged > 0:
		l.Coverage, l.NeedsReview = "none", true
		l.Reason = "Evidence for this turn was purged by the retention policy, so it cannot be labelled."
		l.Signals = append(l.Signals, "evidence_purged")
		return l
	case prompt == "":
		l.Coverage, l.NeedsReview = "none", true
		l.Reason = "The prompt text was not captured on this device, so it cannot be labelled."
		l.Signals = append(l.Signals, "prompt_missing")
		return l
	}
	if purged > 0 {
		l.Coverage = "partial"
	}
	if t.Backfilled {
		l.Signals = append(l.Signals, "backfilled")
		l.Coverage = "partial"
	}

	pw := words(prompt)
	novelty := 1.0
	for _, e := range earlier {
		if sim := jaccard(pw, words(e.Item.Text)); 1-sim < novelty {
			novelty = 1 - sim
		}
	}
	diagnosis := diagnosisRe.MatchString(prompt)
	constraint := constraintRe.MatchString(prompt)
	review := reviewRe.MatchString(prompt)
	specific := specificRe.MatchString(prompt)
	slot := slotRe.MatchString(prompt)
	prevFailed := prev != nil && (prev.EndReason == "interrupted")

	add := func(cond bool, name string) bool {
		if cond {
			l.Signals = append(l.Signals, name)
		}
		return cond
	}
	add(diagnosis, "diagnosis")
	add(constraint, "constraint")
	add(review, "review_or_question")
	add(specific, "specific_reference")
	add(humanEdit, "human_edit_between_turns")
	add(tests > 0, "test_run_observed")
	add(failingTests > 0, "failing_test_observed")
	add(passingTests > 0, "passing_test_observed")
	add(agentDiff, "agent_changed_files")
	add(!t.DiffObserved, "diff_not_observed")
	add(t.EndReason == "interrupted", "interrupted")
	add(prevFailed, "follows_interrupted_turn")
	repeat := add(len(earlier) > 0 && novelty < 0.2, "near_repeat_of_earlier_prompt")

	// Core evidence of effort: the person diagnosed, constrained, reviewed
	// or edited. A green test, a diff or a long prompt alone are not it.
	core := 0
	for _, b := range []bool{diagnosis, constraint, review, humanEdit} {
		if b {
			core++
		}
	}
	switch {
	case slot && !humanEdit:
		l.Value = 0
		l.Reason = "Short re-prompt with no new information or own edits (for example \"try again\")."
		l.Coaching = "Say what you observed and what you think is wrong before asking again."
	case repeat && !humanEdit:
		// A near-identical prompt adds no new hypothesis, even though a pasted
		// error message contains diagnosis words. Changing the hypothesis adds
		// words, which lifts novelty above the threshold.
		l.Value, l.NeedsReview = 0, true
		l.Reason = "Nearly the same as an earlier prompt in this session, with no new words or own edits. Repeating can be legitimate; a reviewer should check."
		l.Coaching = "When you retry, state the changed hypothesis or what you learned from the last attempt."
	case humanEdit:
		l.Value = 1
		l.Reason = "You changed files yourself before this prompt, then directed the agent."
	case core >= 2 || (core >= 1 && (specific || tests > 0)):
		l.Value = 1
		l.Reason = "The prompt shows " + strings.Join(effortWords(diagnosis, constraint, review), " and ") + " grounded in specifics or observed test runs."
	case core == 1:
		l.Value, l.NeedsReview = 1, true
		l.Reason = "One sign of " + strings.Join(effortWords(diagnosis, constraint, review), "") + ", with little else to confirm it."
	default:
		l.Value, l.NeedsReview = 0, true
		l.Reason = "No diagnosis, constraint, review or own edit was observed. This is not a finding of misconduct; a reviewer can credit it with context."
		l.Coaching = "Explain what you expect, what happened instead and which part you want changed."
	}
	if l.Coverage != "full" && l.Value == 1 {
		l.NeedsReview = true
	}
	return l
}

func effortWords(diagnosis, constraint, review bool) []string {
	var out []string
	if diagnosis {
		out = append(out, "diagnosis")
	}
	if constraint {
		out = append(out, "new constraints")
	}
	if review {
		out = append(out, "review")
	}
	return out
}

// dedupe sets ProductiveSeconds for label-1 turns from the union of their
// intervals, giving each overlapping second to the earliest turn.
func dedupe(labels []Label) {
	idx := make([]int, 0, len(labels))
	for i := range labels {
		if labels[i].Value == 1 && labels[i].IntervalEnd > labels[i].IntervalStart {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return labels[idx[a]].IntervalStart < labels[idx[b]].IntervalStart })
	var covered int64
	for _, i := range idx {
		start := max(labels[i].IntervalStart, covered)
		if labels[i].IntervalEnd > start {
			labels[i].ProductiveSeconds = (labels[i].IntervalEnd - start) / 1000
			covered = max(covered, labels[i].IntervalEnd)
		}
	}
}

// humanInterval is the existing two-minute estimate for one prompt, as the
// server computes it from heartbeats: up to two minutes, ending early at the
// next prompt of the session or, without one, when the agent stops.
func humanInterval(t Turn, next *Turn) (start, end int64) {
	start, end = t.StartedAt, t.StartedAt+creditWindow.Milliseconds()
	if next != nil && next.StartedAt < end {
		return start, next.StartedAt
	}
	last := t.LastActivityAt
	if t.EndedAt != nil {
		last = max(last, *t.EndedAt)
	}
	return start, max(start, min(end, last))
}

// labelStandalone labels a turn of the person's own file changes, made with
// no agent prompt. With no prompt text there is no diagnosis to read, so the
// label rests on observed changes alone, which is partial coverage: typing
// cannot be told from a formatter, a generator or an AI assistant inside an
// editor. It is always flagged for a reviewer.
func labelStandalone(t Turn, its []StoredItem, l Label) Label {
	l.Coverage, l.NeedsReview = "partial", true
	l.Signals = []string{"human_edit_standalone", "no_prompt"}
	for _, it := range its {
		if it.Item.Kind == "between_turns_change" {
			l.Evidence = append(l.Evidence, it.ID)
		}
	}
	if len(l.Evidence) == 0 {
		l.Value, l.IntervalEnd = 0, l.IntervalStart
		l.Reason = "No file changes were captured for this turn."
		return l
	}
	l.Value = 1
	l.Reason = "You changed files yourself while no agent was running. There is no prompt, so this rests only on the observed file changes, which cannot tell your typing from a formatter, a generator or an AI assistant inside your editor. A reviewer should confirm it."
	return l
}
