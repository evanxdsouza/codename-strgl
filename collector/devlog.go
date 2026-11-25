// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// DraftDevlog writes a devlog draft for the user to edit and post themselves.
// It reads only the redacted bundle, so it contains nothing the preview has
// not already redacted, and it leaves out items the user excluded or
// redaction withheld. It never shows labels or proposed hours: a devlog is
// public, and the only time it mentions is the activity estimate. The draft
// is not part of the bundle and is never uploaded by the collector.
func DraftDevlog(b *Bundle, loc *time.Location) string {
	type day struct {
		date                 time.Time
		files, ownFiles      []string
		problems             []string
		testsPass, testsFail int
		lastTestPassed       bool
		stopped              int
	}
	var days []*day
	byDate := map[string]*day{}
	for _, t := range b.Turns {
		start := time.UnixMilli(t.StartedAt).In(loc)
		key := start.Format("2006-01-02")
		d := byDate[key]
		if d == nil {
			d = &day{date: start}
			byDate[key] = d
			days = append(days, d)
		}
		signals := map[string]bool{}
		for _, s := range t.Label.Signals {
			signals[s] = true
		}
		d.files = appendUnique(d.files, t.FilesChanged...)
		if t.EndReason == "interrupted" {
			d.stopped++
		}
		// The label's evidence lists test outputs, including Claude Code
		// results whose command is only on the matching tool call.
		evidence := map[string]bool{}
		for _, id := range t.Label.Evidence {
			evidence[id] = true
		}
		for _, it := range t.Items {
			if it.Status != "included" {
				continue
			}
			switch {
			case it.Kind == "between_turns_change":
				// The turn's file list also holds the agent's changes; the
				// person's own files are named on the change's first line.
				first, _, _ := strings.Cut(it.Text, "\n")
				if list, ok := strings.CutPrefix(first, changedFilesPrefix); ok {
					d.ownFiles = appendUnique(d.ownFiles, strings.Split(list, ", ")...)
					d.files = appendUnique(d.files, strings.Split(list, ", ")...)
				}
			case it.Kind == "prompt" && signals["diagnosis"]:
				if s := firstSentence(it.Text); s != "" {
					d.problems = appendUnique(d.problems, s)
				}
			case it.Kind == "tool_output" && evidence[it.ID]:
				failed := it.IsError || (it.Exit != nil && *it.Exit != 0)
				if failed {
					d.testsFail++
				} else {
					d.testsPass++
				}
				d.lastTestPassed = !failed
			}
		}
	}
	sort.Slice(days, func(i, j int) bool { return days[i].date.Before(days[j].date) })

	var w strings.Builder
	fmt.Fprintf(&w, "# %s devlog\n\n", b.Project)
	w.WriteString("_Draft from your local sessions. Edit it before you post it: it is not sent anywhere. Check it for names and details the automatic redaction missed._\n")
	if len(days) == 0 {
		w.WriteString("\nNo sessions recorded in this range.\n")
	}
	for _, d := range days {
		fmt.Fprintf(&w, "\n## %s\n\n", d.date.Format("Mon 2 Jan 2006"))
		if len(d.problems) > 0 {
			w.WriteString("Problems I dug into:\n")
			for _, p := range d.problems {
				fmt.Fprintf(&w, "- %s\n", p)
			}
		}
		if len(d.ownFiles) > 0 {
			fmt.Fprintf(&w, "Changed by hand: %s\n", fileList(d.ownFiles))
		}
		if len(d.files) > 0 {
			fmt.Fprintf(&w, "Files touched: %s\n", fileList(d.files))
		}
		if n := d.testsPass + d.testsFail; n > 0 {
			fmt.Fprintf(&w, "Tests: ran them %d %s (%d failing, %d passing)", n, plural(n, "time", "times"), d.testsFail, d.testsPass)
			if d.lastTestPassed {
				w.WriteString(", passing by the end")
			}
			w.WriteString(".\n")
		}
		if d.testsFail > 0 || d.stopped > 0 {
			w.WriteString("What didn't work: ")
			var parts []string
			if d.testsFail > 0 {
				parts = append(parts, fmt.Sprintf("%d failing test %s", d.testsFail, plural(d.testsFail, "run", "runs")))
			}
			if d.stopped > 0 {
				parts = append(parts, fmt.Sprintf("stopped the agent %d %s to change approach", d.stopped, plural(d.stopped, "time", "times")))
			}
			w.WriteString(strings.Join(parts, "; ") + ". (Add what you learned from it.)\n")
		}
		w.WriteString("Next: (what you plan to try next)\n")
	}
	if b.Totals.EstimatedHumanSeconds > 0 {
		fmt.Fprintf(&w, "\nTracked activity: about %s, an estimate from prompts and edits, not reviewed hours.\n", minutes(b.Totals.EstimatedHumanSeconds))
	}
	return w.String()
}

// firstSentence keeps one sentence of a prompt, at most 140 characters, so
// the draft summarises rather than reproduces it: the first sentence that
// describes a problem, skipping greetings, or else the first sentence.
func firstSentence(text string) string {
	var sentences []string
	rest := strings.TrimSpace(text)
	for rest != "" {
		// A sentence ends at a line break, or at punctuation followed by a
		// space, so "calc.py" or "3.5" does not end it.
		end := len(rest)
		if m := sentenceEndRe.FindStringIndex(rest); m != nil {
			end = m[0] + 1
		}
		if s := strings.TrimSpace(rest[:end]); s != "" {
			sentences = append(sentences, s)
		}
		rest = strings.TrimSpace(rest[end:])
	}
	if len(sentences) == 0 {
		return ""
	}
	text = sentences[0]
	for _, s := range sentences {
		if diagnosisRe.MatchString(s) {
			text = s
			break
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) > 140 {
		r := []rune(text)
		text = strings.TrimSpace(string(r[:139])) + "…"
	}
	return text
}

var sentenceEndRe = regexp.MustCompile(`[.?!]\s|\n`)

func fileList(files []string) string {
	const max = 6
	if len(files) <= max {
		return strings.Join(files, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(files[:max], ", "), len(files)-max)
}

func appendUnique(list []string, values ...string) []string {
	for _, v := range values {
		found := false
		for _, have := range list {
			if have == v {
				found = true
				break
			}
		}
		if !found && v != "" {
			list = append(list, v)
		}
	}
	return list
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func minutes(seconds int64) string {
	if seconds < 3600 {
		return fmt.Sprintf("%d min", (seconds+30)/60)
	}
	return fmt.Sprintf("%.1f h", float64(seconds)/3600)
}
