// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// BundleSchema is the version of the submission format below.
const BundleSchema = 1

// Bundle is exactly what leaves the device when a user submits hours: turn
// metadata, proposed labels, redacted evidence and the commitments that tie
// each redacted item to the raw item recorded earlier. Raw text is never in
// it. The preview shows this structure, and the uploaded bytes are the
// preview's bytes.
type Bundle struct {
	Schema           int          `json:"schema"`
	CollectorVersion string       `json:"collector_version"`
	DeviceID         string       `json:"device_id"`
	KeyProtection    string       `json:"key_protection"`
	RuleSet          string       `json:"rule_set"`
	RedactionVersion string       `json:"redaction_version"`
	Project          string       `json:"project"`
	From             int64        `json:"from"` // milliseconds since the Unix epoch, inclusive
	To               int64        `json:"to"`   // exclusive
	CreatedAt        int64        `json:"created_at"`
	UserContext      string       `json:"user_context"`
	Supersedes       string       `json:"supersedes,omitempty"` // digest of a submission sent back for relabelling
	Totals           Totals       `json:"totals"`
	Turns            []BundleTurn `json:"turns"`
	Chain            BundleChain  `json:"chain"`
	Notes            []string     `json:"notes"`
}

// Totals keeps the three numbers apart: the activity estimate, agent-only
// time and the proposed productive time. Approval happens on the server.
type Totals struct {
	EstimatedHumanSeconds     int64 `json:"estimated_human_seconds"`
	AgentSeconds              int64 `json:"agent_seconds"`
	ProposedProductiveSeconds int64 `json:"proposed_productive_seconds"`
	Turns                     int   `json:"turns"`
	LabelledOne               int   `json:"labelled_one"`
	NeedsReview               int   `json:"needs_review"`
	Backfilled                int   `json:"backfilled"`
	WithheldItems             int   `json:"withheld_items"`
	ExcludedItems             int   `json:"excluded_items"`
}

type BundleTurn struct {
	ID            string       `json:"id"`
	Harness       string       `json:"harness"`
	Session       string       `json:"session"` // salted hash of the harness session ID
	Branch        string       `json:"branch"`
	Model         string       `json:"model"`
	StartedAt     int64        `json:"started_at"`
	EndedAt       *int64       `json:"ended_at"`
	EndReason     string       `json:"end_reason"`
	RecordedAt    int64        `json:"recorded_at"`
	Backfilled    bool         `json:"backfilled"`
	PromptLength  int          `json:"prompt_length"`
	AgentEvents   int          `json:"agent_events"`
	ToolCalls     int          `json:"tool_calls"`
	ChildSessions int          `json:"child_sessions"`
	InputTokens   int64        `json:"input_tokens"`
	OutputTokens  int64        `json:"output_tokens"`
	FilesChanged  []string     `json:"files_changed"`
	DiffObserved  bool         `json:"diff_observed"`
	Label         Label        `json:"label"`
	Items         []BundleItem `json:"items"`
}

// BundleItem is one evidence item. Status is included, excluded (the user
// removed it), withheld (redaction verification failed, so nothing was
// sent) or purged (deleted locally by the retention policy).
type BundleItem struct {
	ID         string      `json:"id"`
	Kind       string      `json:"kind"`
	OccurredAt int64       `json:"occurred_at"`
	RecordedAt int64       `json:"recorded_at"`
	Commitment string      `json:"commitment"`
	ChainSeq   int64       `json:"chain_seq"`
	Status     string      `json:"status"`
	Tool       string      `json:"tool,omitempty"`
	Command    string      `json:"command,omitempty"`
	Exit       *int        `json:"exit,omitempty"`
	IsError    bool        `json:"is_error,omitempty"`
	Text       string      `json:"text,omitempty"`
	Redactions []Redaction `json:"redactions,omitempty"`
}

// BundleChain lets the server recompute every chain head between the first
// and last submitted item and compare them with heads it received earlier.
type BundleChain struct {
	PreviousHead string       `json:"previous_head"`
	Entries      []ChainEntry `json:"entries"`
}

// BuildOptions are the user's choices in the preview.
type BuildOptions struct {
	Project     string
	From, To    time.Time
	Exclude     map[string]bool // item IDs
	UserContext string
	Supersedes  string
	Terms       []string // extra words to redact
	Now         time.Time
}

const maxItemText = 8000

// BuildBundle labels the range's turns and assembles a redacted bundle. It
// fails closed: an item whose redacted text still matches a sensitive
// pattern, or any redacted field that does, is withheld entirely.
func (s *Store) BuildBundle(opt BuildOptions) (*Bundle, error) {
	turns, err := s.Turns(opt.Project, opt.From, opt.To)
	if err != nil {
		return nil, err
	}
	labels, items, err := s.labelTurns(turns)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	if home, err := os.UserHomeDir(); err == nil {
		paths[home] = "~"
	}
	for _, t := range turns {
		if t.root != "" {
			paths[t.root] = "<" + t.Project + ">"
		}
	}
	r := NewRedactor(s, opt.Terms).WithPaths(paths)
	userContext, _, ok := r.Redact(opt.UserContext)
	if !ok {
		return nil, fmt.Errorf("the added context still contains text that looks sensitive after redaction; edit it and try again")
	}
	b := &Bundle{
		Schema: BundleSchema, CollectorVersion: version, DeviceID: s.DeviceID, KeyProtection: s.KeyProtection,
		RuleSet: RuleSetVersion, RedactionVersion: RedactionVersion, Project: opt.Project,
		From: opt.From.UnixMilli(), To: opt.To.UnixMilli(), CreatedAt: opt.Now.UnixMilli(),
		UserContext: userContext, Supersedes: opt.Supersedes, Turns: []BundleTurn{},
		Notes: []string{
			"Automated redaction (" + RedactionVersion + ") replaces common secret formats and contact details with typed placeholders. It is not guaranteed anonymisation.",
			"Only file changes are observed when no prompt is sent: reading, research and thinking leave no trace the collector sees, and changes by an AI assistant inside an editor look like the person's own.",
			"Commitments are salted hashes of the raw items, recorded on the device when the items were captured. They protect against later alteration, not against a modified client.",
		},
	}
	minSeq, maxSeq := int64(0), int64(0)
	for i, t := range turns {
		l := labels[i]
		bt := BundleTurn{
			ID: t.ID, Harness: t.Harness, Session: s.mac("session", t.Harness, t.Session)[:16], Branch: t.Branch, Model: t.Model,
			StartedAt: t.StartedAt, EndedAt: t.EndedAt, EndReason: t.EndReason, RecordedAt: t.RecordedAt, Backfilled: t.Backfilled,
			PromptLength: t.PromptLength, AgentEvents: t.AgentEvents, ToolCalls: t.ToolCalls, ChildSessions: t.ChildSessions,
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, FilesChanged: redactList(r, t.ChangedFiles), DiffObserved: t.DiffObserved,
			Label: l, Items: []BundleItem{},
		}
		for _, it := range items[t.ID] {
			bi := BundleItem{ID: it.ID, Kind: it.Item.Kind, OccurredAt: it.OccurredAt, RecordedAt: it.RecordedAt, Commitment: it.Commitment, ChainSeq: it.ChainSeq}
			if minSeq == 0 || it.ChainSeq < minSeq {
				minSeq = it.ChainSeq
			}
			maxSeq = max(maxSeq, it.ChainSeq)
			switch {
			case it.Purged:
				bi.Status = "purged"
			case opt.Exclude[it.ID]:
				bi.Status = "excluded"
				b.Totals.ExcludedItems++
			default:
				text := it.Item.Text
				if len(text) > maxItemText {
					text = text[:maxItemText] + "\n[truncated: " + fmt.Sprint(len(it.Item.Text)-maxItemText) + " more bytes kept on the device]"
				}
				red, found, ok1 := r.Redact(text)
				cmd, found2, ok2 := r.Redact(it.Item.Command)
				tool, _, ok3 := r.Redact(it.Item.Tool)
				if !ok1 || !ok2 || !ok3 {
					bi.Status = "withheld"
					b.Totals.WithheldItems++
					break
				}
				bi.Status, bi.Text, bi.Command, bi.Tool = "included", red, cmd, tool
				bi.Exit, bi.IsError, bi.Redactions = it.Item.Exit, it.Item.IsError, append(found, found2...)
			}
			bt.Items = append(bt.Items, bi)
		}
		b.Turns = append(b.Turns, bt)
		b.Totals.Turns++
		b.Totals.LabelledOne += l.Value
		if l.NeedsReview {
			b.Totals.NeedsReview++
		}
		if t.Backfilled {
			b.Totals.Backfilled++
		}
		b.Totals.ProposedProductiveSeconds += l.ProductiveSeconds
	}
	b.Totals.EstimatedHumanSeconds, b.Totals.AgentSeconds = estimate(turns)
	if minSeq > 0 {
		prev, entries, err := s.chainSegment(minSeq, maxSeq)
		if err != nil {
			return nil, err
		}
		b.Chain = BundleChain{PreviousHead: prev, Entries: entries}
	} else {
		b.Chain.Entries = []ChainEntry{}
	}
	return b, nil
}

func redactList(r *Redactor, list []string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		red, _, ok := r.Redact(v)
		if !ok {
			red = "[WITHHELD]"
		}
		out = append(out, red)
	}
	return out
}

// estimate applies the existing two-minute rule to the turns: each prompt
// credits up to two minutes of human time, deduplicated across sessions, and
// the rest of each turn's span is agent time.
func estimate(turns []Turn) (human, agent int64) {
	type span struct{ start, end int64 }
	var humanSpans, all []span
	for i, t := range turns {
		end := t.LastActivityAt
		if t.EndedAt != nil {
			end = max(end, *t.EndedAt)
		}
		all = append(all, span{t.StartedAt, end})
		if t.PromptLength == 0 && t.Harness != standaloneHarness {
			continue
		}
		var next *Turn
		for j := i + 1; j < len(turns); j++ {
			if turns[j].Session == t.Session && turns[j].Harness == t.Harness {
				next = &turns[j]
				break
			}
		}
		_, hEnd := humanInterval(t, next)
		humanSpans = append(humanSpans, span{t.StartedAt, hEnd})
	}
	union := func(spans []span) int64 {
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		var total, covered int64
		for _, sp := range spans {
			start := max(sp.start, covered)
			if sp.end > start {
				total += sp.end - start
				covered = sp.end
			}
		}
		return total / 1000
	}
	human = union(humanSpans)
	agent = max(0, union(append(all, humanSpans...))-human)
	return human, agent
}

// Encode returns the bundle's exact upload bytes and their SHA-256 digest.
func (b *Bundle) Encode() ([]byte, string) {
	data, _ := json.MarshalIndent(b, "", "  ")
	data = append(data, '\n')
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}
