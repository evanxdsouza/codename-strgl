// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// redactEvalCase is one annotated example. Sensitive values must not survive
// redaction; keep values (code, errors, library names) must survive unchanged.
type redactEvalCase struct {
	Text      string `json:"text"`
	Sensitive []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"sensitive"`
	Keep []string `json:"keep"`
}

// redactEvalSets are the corpora and the recall each type must reach under
// the current RedactionVersion. redact_eval.json is the development set the
// rules were written against; redact_eval_holdout.json was written before it
// was measured, and only one password rule was fixed after seeing it. Both are written by us,
// not taken from real users, so the floors catch regressions; they are not a
// real-world recall figure. Raise a floor when the rules improve; never lower
// one to make a change pass.
var redactEvalSets = []struct {
	file    string
	floor   map[string]float64
	minKept float64 // share of keep values that must survive
}{
	{"testdata/redact_eval.json", map[string]float64{
		"ADDRESS": 1, "DOB": 1, "EMAIL": 1, "HANDLE": 0.83, "IP": 1, "PERSON": 0.7,
		"PHONE": 1, "PLACE": 0.5, "SCHOOL": 1, "SECRET": 1,
	}, 1.0},
	{"testdata/redact_eval_holdout.json", map[string]float64{
		"ADDRESS": 1, "DOB": 1, "EMAIL": 1, "HANDLE": 0.5, "PERSON": 0.75,
		// PLACE fell from 0.66 when "Leeds" was added with the UK cases; a
		// bare city name is the entity model's job, not the rules'.
		"PHONE": 1, "PLACE": 0.5, "SCHOOL": 1, "SECRET": 1,
	}, 1.0},
	// Written after the rules and used only to compare entity models.
	{"testdata/redact_eval_holdout2.json", map[string]float64{
		"ADDRESS": 1, "DOB": 1, "EMAIL": 1, "IP": 1, "PERSON": 0.6,
		"PHONE": 1, "PLACE": 0.2, "SCHOOL": 1, "SECRET": 1,
	}, 1.0},
}

// nerRecallTarget is the recall an optional entity model must reach on both
// sets before it is accepted (decided 2026-11-25): 99% for secrets and
// contact details, 90% for names, schools and places. It is checked only
// when STRGL_NER_HELPER names a helper; the rules alone do not reach it.
var nerRecallTarget = map[string]float64{
	"SECRET": 0.99, "EMAIL": 0.99, "PHONE": 0.99, "ADDRESS": 0.99, "DOB": 0.99, "IP": 0.99,
	"PERSON": 0.90, "SCHOOL": 0.90, "PLACE": 0.90,
}

// nerAcceptedBelowTarget records shortfalls the user accepted on 2026-11-25:
// dslim/distilbert-NER ships as an opt-in helper although it misses the
// name and place targets on holdout2, which has only 10 names and 5 places
// and is synthetic. The
// test still fails if the model falls below these figures, and every other
// model must meet nerRecallTarget in full.
var nerAcceptedBelowTarget = map[string]float64{
	"dslim/distilbert-NER redact_eval_holdout2 PERSON": 0.8,
	// One of only five places missed ("Mississauga"); same grounds as names.
	"dslim/distilbert-NER redact_eval_holdout2 PLACE": 0.8,
}

// nerMinKept is the share of benign text that must survive with a model. A
// model removes some harmless words, such as "Newton" in "Newton's method";
// that loses evidence for the reviewer but leaks nothing.
const nerMinKept = 0.95

// TestRedactionRecall measures how much annotated personal data each type
// of rule removes, and how much benign text it destroys. Run with -v to see
// the table.
func TestRedactionRecall(t *testing.T) {
	store, _ := openTestStore(t)
	r := NewRedactor(store, nil)
	if cmd := os.Getenv("STRGL_NER_HELPER"); cmd != "" {
		h, err := StartNER(cmd)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		r.WithNER(h)
	}
	for _, set := range redactEvalSets {
		name := strings.TrimSuffix(strings.TrimPrefix(set.file, "testdata/"), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(set.file)
			if err != nil {
				t.Fatal(err)
			}
			var cases []redactEvalCase
			if err := json.Unmarshal(raw, &cases); err != nil {
				t.Fatal(err)
			}
			caught, total := map[string]int{}, map[string]int{}
			kept, keepTotal, withheld := 0, 0, 0
			for _, c := range cases {
				out, _, ok := r.Redact(c.Text)
				if !ok {
					// The bundle withholds the whole item: nothing leaks, nothing is kept.
					withheld++
					t.Logf("withheld %q", out)
					out = ""
				}
				low := strings.ToLower(out)
				for _, s := range c.Sensitive {
					total[s.Type]++
					if !strings.Contains(low, strings.ToLower(s.Value)) {
						caught[s.Type]++
					} else {
						t.Logf("missed %s %q", s.Type, s.Value)
					}
				}
				for _, k := range c.Keep {
					keepTotal++
					if strings.Contains(out, k) {
						kept++
					} else if ok {
						t.Logf("over-redacted %q in %q", k, out)
					}
				}
			}
			types := make([]string, 0, len(total))
			for k := range total {
				types = append(types, k)
			}
			sort.Strings(types)
			t.Logf("%s on %d cases (%d withheld):", r.Version(), len(cases), withheld)
			for _, k := range types {
				recall := float64(caught[k]) / float64(total[k])
				t.Logf("  %-8s recall %5.1f%% (%d/%d)", k, 100*recall, caught[k], total[k])
				if recall < set.floor[k] {
					t.Errorf("%s recall %.3f below floor %.3f", k, recall, set.floor[k])
				}
				if r.ner != nil {
					target := nerRecallTarget[k]
					if accepted, ok := nerAcceptedBelowTarget[r.ner.Model+" "+name+" "+k]; ok {
						target = accepted
						t.Logf("  %-8s accepted below target: floor %.2f instead of %.2f", k, accepted, nerRecallTarget[k])
					}
					if recall < target {
						t.Errorf("%s recall %.3f below the entity-model target %.3f", k, recall, target)
					}
				}
			}
			keptFraction := float64(kept) / float64(keepTotal)
			t.Logf("  benign text kept %5.1f%% (%d/%d)", 100*keptFraction, kept, keepTotal)
			minKept := set.minKept
			if r.ner != nil {
				minKept = min(minKept, nerMinKept)
			}
			if keptFraction < minKept {
				t.Errorf("benign text kept %.3f below %.3f", keptFraction, minKept)
			}
		})
	}
}

// TestHelperNER is not a test: TestNERRedactsAndFailsClosed runs the test
// binary as a fake entity-model helper with STRGL_FAKE_NER set.
func TestHelperNER(t *testing.T) {
	mode := os.Getenv("STRGL_FAKE_NER")
	if mode == "" {
		return
	}
	fmt.Println(`{"model":"fake"}`)
	if mode == "die" {
		os.Exit(0)
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var req struct{ Text string }
		_ = json.Unmarshal(in.Bytes(), &req)
		var out []nerEntity
		for _, name := range []string{"Grace", "Grace Lee", "Omar"} {
			if strings.Contains(req.Text, name) {
				out = append(out, nerEntity{"PERSON", name})
			}
		}
		// A malformed type must not break the placeholder format.
		out = append(out, nerEntity{"b-loc", "Valley"})
		data, _ := json.Marshal(map[string]any{"entities": out})
		fmt.Println(string(data))
	}
	os.Exit(0)
}

func TestNERRedactsAndFailsClosed(t *testing.T) {
	store, _ := openTestStore(t)
	start := func(mode string) *NERHelper {
		t.Helper()
		t.Setenv("STRGL_FAKE_NER", mode)
		h, err := StartNER(os.Args[0] + " -test.run=^TestHelperNER$")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.Close)
		return h
	}
	r := NewRedactor(store, nil).WithNER(start("ok"))
	out, found, ok := r.Redact("Built by Omar and Grace Lee at Hack the Valley; Grace wrote GraceNote. key=" + "sk-live-" + strings.Repeat("a1B2", 6))
	if !ok || strings.Contains(out, "Grace ") || strings.Contains(out, "Omar") || strings.Contains(out, "Valley") || !strings.Contains(out, "GraceNote") {
		t.Fatalf("redacted: %q %v", out, found)
	}
	if !strings.Contains(out, "[NAME:") || r.Version() != RedactionVersion+"+ner:fake" {
		t.Fatalf("type or version: %q %s", out, r.Version())
	}
	// A helper that stops answering withholds the item and every later one.
	dead := NewRedactor(store, nil).WithNER(start("die"))
	for range 2 {
		if _, _, ok := dead.Redact("hello Grace"); ok {
			t.Fatal("redaction succeeded without the enabled model")
		}
	}
}
