// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "evidence.db"), 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

// A Claude Code session: a prompt with a secret, a failing test run, an
// injected slash-command line and a subagent line (neither is a prompt), a
// second prompt that only says "try again", then an interruption.
func claudeSession(cwd string) []string {
	return []string{
		claudeLine("user", 0, cwd, `,"message":{"role":"user","content":"why does add() fail in calc.py? my key is sk-live-`+strings.Repeat("a1B2", 6)+` and email me at kid@example.com"}`),
		claudeLine("assistant", 5, cwd, `,"message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"Running the tests."},{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"python3 -m unittest"}}],"usage":{"input_tokens":100,"output_tokens":10}}`),
		claudeLine("user", 6, cwd, `,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"FAIL: test_add\nAssertionError: -1 != 5","is_error":true}]}`),
		claudeLine("user", 7, cwd, `,"message":{"role":"user","content":"<command-name>/cost</command-name>"}`),
		strings.Replace(claudeLine("user", 8, cwd, `,"message":{"role":"user","content":"subagent instruction"}`), `"isSidechain":false`, `"isSidechain":true`, 1),
		claudeLine("assistant", 20, cwd, `,"message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"add subtracts instead of adding."}],"usage":{"input_tokens":50,"output_tokens":5}}`),
		claudeLine("user", 60, cwd, `,"message":{"role":"user","content":"try again"}`),
		claudeLine("assistant", 70, cwd, `,"message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"Retrying."}],"usage":{"input_tokens":5,"output_tokens":1}}`),
		claudeLine("user", 75, cwd, `,"message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}`),
	}
}

func TestTimelineTurnsEvidenceAndRedactedBundle(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	transcript := filepath.Join(home, "projects", "-x", "s1.jsonl")
	write(t, transcript, claudeSession(cwd)...)
	store, dir := openTestStore(t)
	src := []captureSource{{"claude-code", filepath.Join(home, "projects"), captureClaudeLine}}
	now := t0.Add(80 * time.Second) // observed live, not backfilled

	if _, err := store.CaptureScan(src, t0.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	// A second scan, and a restarted scan of a rewritten file, add nothing.
	if _, err := store.CaptureScan(src, t0.Add(-time.Hour), now); err != nil {
		t.Fatal(err)
	}
	turns, err := store.Turns("terminet", t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("want 2 turns (injected and subagent lines are not prompts), got %d: %+v", len(turns), turns)
	}
	first, second := turns[0], turns[1]
	if first.EndReason != "superseded" || first.ToolCalls != 1 || first.InputTokens != 150 || first.Backfilled {
		t.Fatalf("first turn: %+v", first)
	}
	if second.EndReason != "interrupted" || second.PromptLength != len("try again") {
		t.Fatalf("second turn: %+v", second)
	}

	// Raw text is encrypted at rest.
	raw, _ := os.ReadFile(filepath.Join(dir, "evidence.db"))
	wal, _ := os.ReadFile(filepath.Join(dir, "evidence.db-wal"))
	if bytes.Contains(append(raw, wal...), []byte("kid@example.com")) || bytes.Contains(append(raw, wal...), []byte("AssertionError")) {
		t.Fatal("evidence text stored unencrypted")
	}

	b, err := store.BuildBundle(BuildOptions{Project: "terminet", From: t0.Add(-time.Hour), To: t0.Add(time.Hour), Now: now, UserContext: "I am learning Python"})
	if err != nil {
		t.Fatal(err)
	}
	data, digest := b.Encode()
	for _, secret := range []string{"kid@example.com", "a1B2a1B2a1B2", filepath.Dir(filepath.Dir(cwd))} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("bundle leaks %q", secret)
		}
	}
	if !bytes.Contains(data, []byte("[EMAIL:")) || !bytes.Contains(data, []byte("[API_KEY:")) || !bytes.Contains(data, []byte("AssertionError")) {
		t.Fatalf("bundle lost useful evidence or placeholders:\n%s", data)
	}
	if len(digest) != 64 {
		t.Fatal("no digest")
	}
	l1, l2 := b.Turns[0].Label, b.Turns[1].Label
	if l1.Value != 1 || !contains(l1.Signals, "diagnosis") || !contains(l1.Signals, "failing_test_observed") {
		t.Fatalf("diagnosis turn label: %+v", l1)
	}
	if l2.Value != 0 || l2.Coaching == "" {
		t.Fatalf("slot-machine re-prompt label: %+v", l2)
	}
	// 60 seconds until the next prompt, then 15 until the interruption: the
	// two-minute window ends early, as on the server.
	if l1.ProductiveSeconds != 60 || b.Totals.ProposedProductiveSeconds != 60 || b.Totals.EstimatedHumanSeconds != 75 {
		t.Fatalf("seconds: label %d, totals %+v", l1.ProductiveSeconds, b.Totals)
	}
	// The chain segment lets a server recompute heads.
	head := b.Chain.PreviousHead
	for _, e := range b.Chain.Entries {
		head = chainHead(head, e.Commitment, e.RecordedAt)
	}
	var stored string
	_ = store.db.QueryRow(`SELECT head FROM chain ORDER BY seq DESC LIMIT 1`).Scan(&stored)
	if head != stored {
		t.Fatal("chain segment does not reproduce the head")
	}

	// Consent to a model re-check is off unless the user gives it, and is
	// part of the frozen bytes when they do.
	if bytes.Contains(data, []byte("model_relabel_consent")) {
		t.Fatal("bundle carries model consent the user did not give")
	}
	consented, _ := store.BuildBundle(BuildOptions{Project: "terminet", From: t0.Add(-time.Hour), To: t0.Add(time.Hour), Now: now, UserContext: "I am learning Python", AllowModelRelabel: true})
	if cdata, cdigest := consented.Encode(); !bytes.Contains(cdata, []byte(`"model_relabel_consent": true`)) && !bytes.Contains(cdata, []byte(`"model_relabel_consent":true`)) || cdigest == digest {
		t.Fatalf("consent missing from the bundle or not covered by its digest:\n%s", cdata)
	}

	// Excluding an item leaves only its commitment in the bundle.
	excluded := b.Turns[0].Items[0].ID
	b2, _ := store.BuildBundle(BuildOptions{Project: "terminet", From: t0.Add(-time.Hour), To: t0.Add(time.Hour), Now: now, Exclude: map[string]bool{excluded: true}})
	if it := b2.Turns[0].Items[0]; it.Status != "excluded" || it.Text != "" || it.Commitment == "" {
		t.Fatalf("excluded item: %+v", it)
	}

	// The devlog draft summarises the diagnosis, keeps the failed test run
	// as part of the work and never shows labels or proposed hours.
	draft := DraftDevlog(b, time.UTC)
	for _, want := range []string{"- why does add() fail in calc.py?\n", "1 failing, 0 passing", "stopped the agent 1 time", "about 1 min, an estimate"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("draft lacks %q:\n%s", want, draft)
		}
	}
	for _, unwanted := range []string{"try again", "label", "productive", "kid@example.com"} {
		if strings.Contains(strings.ToLower(draft), unwanted) {
			t.Fatalf("draft contains %q:\n%s", unwanted, draft)
		}
	}
	// An item the user left out of the bundle is left out of the draft too.
	if strings.Contains(DraftDevlog(b2, time.UTC), "why does add()") {
		t.Fatal("draft shows an excluded prompt")
	}
}

func TestRedactionFailsClosedAndUserTerms(t *testing.T) {
	store, _ := openTestStore(t)
	r := NewRedactor(store, []string{"Riverdale High"})
	out, found, ok := r.Redact("I go to riverdale high; token=hunter2hunter2 ghp_" + strings.Repeat("x", 30) + " at /home/alice/proj, call +1 415 555 0199")
	if !ok || strings.Contains(strings.ToLower(out), "riverdale") || strings.Contains(out, "hunter2") || strings.Contains(out, "alice") || strings.Contains(out, "555") {
		t.Fatalf("redaction missed something: %q %v", out, found)
	}
	// Same value, same placeholder; ordinary code survives.
	a, _, _ := r.Redact("kid@example.com")
	b, _, _ := r.Redact("mail kid@example.com")
	if !strings.HasSuffix(b, a) {
		t.Fatalf("placeholders differ: %q %q", a, b)
	}
	code := "func add(a, b int) int { return a + b } // see calc_test.go line 12"
	if got, _, _ := r.Redact(code); got != code {
		t.Fatalf("code changed: %q", got)
	}
	// A rule that cannot fully remove a match must report failure.
	if r.clean("password: stillhere") {
		t.Fatal("verification accepted remaining secret")
	}
}

func TestLabelDedupesOverlappingTurns(t *testing.T) {
	ms := t0.UnixMilli()
	labels := []Label{
		{Value: 1, IntervalStart: ms, IntervalEnd: ms + 120_000},
		{Value: 1, IntervalStart: ms + 60_000, IntervalEnd: ms + 180_000}, // parallel session
		{Value: 0, IntervalStart: ms + 200_000, IntervalEnd: ms + 320_000},
		{Value: 1, IntervalStart: ms + 10_000, IntervalEnd: ms + 50_000}, // inside the first
	}
	dedupe(labels)
	got := []int64{labels[0].ProductiveSeconds, labels[1].ProductiveSeconds, labels[2].ProductiveSeconds, labels[3].ProductiveSeconds}
	if got[0]+got[1]+got[3] != 180 || got[2] != 0 {
		t.Fatalf("productive seconds %v, want 180 in total for the union and 0 for the label-0 turn", got)
	}
}

func TestOpenCodeEvidenceSpoolChildrenJoinParentTurn(t *testing.T) {
	_, cwd := repo(t)
	dir := t.TempDir()
	rec := func(fields map[string]any) string {
		base := map[string]any{"schema": 1, "cwd": cwd, "model": "big-pickle", "version": "2.3.2"}
		for k, v := range fields {
			base[k] = v
		}
		b, _ := json.Marshal(base)
		return string(b)
	}
	ms := t0.UnixMilli()
	write(t, filepath.Join(dir, "parent.jsonl"),
		rec(map[string]any{"session_id": "p", "sequence": 1, "timestamp": ms, "kind": "prompt", "text": "the test fails because add subtracts; fix calc.py only"}),
		rec(map[string]any{"session_id": "p", "sequence": 2, "timestamp": ms + 1000, "kind": "tool_call", "tool": "shell", "tool_id": "c1", "input": map[string]any{"command": "python3 -m unittest"}}),
		rec(map[string]any{"session_id": "p", "sequence": 3, "timestamp": ms + 2000, "kind": "tool_output", "tool": "shell", "tool_id": "c1", "text": "OK", "exit": 0}),
		rec(map[string]any{"session_id": "p", "sequence": 3, "timestamp": ms + 2000, "kind": "tool_output", "tool": "shell", "tool_id": "c1", "text": "OK", "exit": 0}), // replayed
		rec(map[string]any{"session_id": "p", "sequence": 4, "timestamp": ms + 9000, "kind": "end"}),
	)
	write(t, filepath.Join(dir, "child.jsonl"),
		rec(map[string]any{"session_id": "c", "parent_id": "p", "sequence": 1, "timestamp": ms + 3000, "kind": "response", "text": "child instruction"}),
		rec(map[string]any{"session_id": "c", "parent_id": "p", "sequence": 2, "timestamp": ms + 4000, "kind": "end", "interrupted": true}),
	)
	store, _ := openTestStore(t)
	if _, err := store.CaptureScan([]captureSource{{"opencode", dir, captureOpenCodeLine}}, t0.Add(-time.Hour), t0.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	turns, _ := store.Turns("", t0.Add(-time.Hour), t0.Add(time.Hour))
	if len(turns) != 1 {
		t.Fatalf("child session made its own turn: %+v", turns)
	}
	if tt := turns[0]; tt.ChildSessions != 1 || tt.EndReason != "completed" || tt.ToolCalls != 1 {
		t.Fatalf("turn: %+v", tt)
	}
	items, _ := store.itemsForTurn(turns[0].ID)
	if len(items) != 4 {
		t.Fatalf("want prompt, call, output and child text once each, got %d", len(items))
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func TestScanSnapshotsAttributeAgentAndHumanChanges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "calcs")
	run := func(args ...string) {
		t.Helper()
		if out, err := git(root, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write(t, filepath.Join(root, "calc.py"), "def add(a, b):", "    return a - b")
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	run("add", ".")
	run("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init")

	home := t.TempDir()
	transcript := filepath.Join(home, "projects", "-x", "s1.jsonl")
	appendLines := func(lines ...string) {
		f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		defer f.Close()
		for _, l := range lines {
			f.WriteString(l + "\n")
		}
	}
	os.MkdirAll(filepath.Dir(transcript), 0o755)
	store, _ := openTestStore(t)
	src := []captureSource{{"claude-code", filepath.Join(home, "projects"), captureClaudeLine}}
	scanAt := func(seconds int) {
		t.Helper()
		if _, err := store.CaptureScan(src, t0.Add(-time.Hour), t0.Add(time.Duration(seconds)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	appendLines(claudeLine("user", 0, root, `,"message":{"role":"user","content":"make add() pass the test"}`),
		claudeLine("assistant", 5, root, `,"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	scanAt(6)                                                                      // baseline snapshot
	write(t, filepath.Join(root, "calc.py"), "def add(a, b):", "    return a + b") // the agent's edit
	appendLines(claudeLine("assistant", 30, root, `,"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"fixed"}]}`))
	scanAt(31)
	scanAt(200)                                                                                                        // the turn goes idle
	write(t, filepath.Join(root, "calc.py"), "def add(a, b):", "    return a + b", "", "def neg(a):", "    return -a") // the person's edit
	scanAt(260)
	appendLines(claudeLine("user", 270, root, `,"message":{"role":"user","content":"I added neg(); write a test for it"}`))
	scanAt(271)

	turns, _ := store.Turns("calcs", t0.Add(-time.Hour), t0.Add(time.Hour))
	if len(turns) != 2 {
		t.Fatalf("turns: %+v", turns)
	}
	if !turns[0].DiffObserved || len(turns[0].ChangedFiles) != 1 || turns[0].EndReason != "idle" {
		t.Fatalf("agent turn: %+v", turns[0])
	}
	kinds := func(id string) []string {
		items, _ := store.itemsForTurn(id)
		var out []string
		for _, it := range items {
			out = append(out, it.Item.Kind)
		}
		return out
	}
	if k := kinds(turns[0].ID); !contains(k, "diff") || contains(k, "between_turns_change") {
		t.Fatalf("agent turn items: %v", k)
	}
	if k := kinds(turns[1].ID); !contains(k, "between_turns_change") {
		t.Fatalf("the person's edit was not attached to their next prompt: %v", k)
	}
	// The person's change shows only their lines, not the agent's earlier fix.
	items, _ := store.itemsForTurn(turns[1].ID)
	for _, it := range items {
		if it.Item.Kind == "between_turns_change" {
			if !strings.Contains(it.Item.Text, "+def neg(a):") || strings.Contains(it.Item.Text, "a - b") || strings.Contains(it.Item.Text, "+    return a + b") {
				t.Fatalf("human change diff: %q", it.Item.Text)
			}
		}
	}
	labels, _, _ := store.labelTurns(turns)
	if labels[1].Value != 1 || !contains(labels[1].Signals, "human_edit_between_turns") {
		t.Fatalf("label after own edit: %+v", labels[1])
	}
	if contains(labels[0].Signals, "human_edit_between_turns") {
		t.Fatal("the agent's edit was credited as the person's")
	}

	// The agent then writes the test during the person's turn. The devlog
	// credits only calc.py as changed by hand.
	write(t, filepath.Join(root, "test_calc.py"), "from calc import neg", "assert neg(2) == -2")
	appendLines(claudeLine("assistant", 290, root, `,"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"added"}]}`))
	scanAt(291)
	b, err := store.BuildBundle(BuildOptions{Project: filepath.Base(root), From: t0.Add(-time.Hour), To: t0.Add(time.Hour), Now: t0.Add(300 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	draft := DraftDevlog(b, time.UTC)
	if !strings.Contains(draft, "Changed by hand: calc.py\n") || !strings.Contains(draft, "test_calc.py") {
		t.Fatalf("draft:\n%s", draft)
	}
}

func TestSnapshotHashesDoNotDependOnOtherFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "r")
	write(t, filepath.Join(root, "a.txt"), "one")
	write(t, filepath.Join(root, "b.txt"), "two")
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	git(root, "add", ".")
	git(root, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init")
	write(t, filepath.Join(root, "a.txt"), "one changed")
	before, _ := takeSnapshot(root, t0)
	write(t, filepath.Join(root, "b.txt"), "two changed")
	after, _ := takeSnapshot(root, t0)
	if got := changedFiles(before, after); len(got) != 1 || got[0] != "b.txt" {
		t.Fatalf("changed files %v, want only b.txt", got)
	}
}

// Editing with no prompt must not vanish: changes in a repository that then
// goes quiet become a standalone turn that rests on the changes alone, is
// flagged for review and never credits more than the two-minute estimate.
func TestStandaloneEditsBecomeAFlaggedTurn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "calcs")
	write(t, filepath.Join(root, "calc.py"), "def add(a, b):", "    return a + b")
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	git(root, "add", ".")
	git(root, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init")

	home := t.TempDir()
	transcript := filepath.Join(home, "projects", "-x", "s1.jsonl")
	write(t, transcript,
		claudeLine("user", 0, root, `,"message":{"role":"user","content":"explain add()"}`),
		claudeLine("assistant", 5, root, `,"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	store, _ := openTestStore(t)
	src := []captureSource{{"claude-code", filepath.Join(home, "projects"), captureClaudeLine}}
	scanAt := func(seconds int) {
		t.Helper()
		if _, err := store.CaptureScan(src, t0.Add(-time.Hour), t0.Add(time.Duration(seconds)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	scanAt(6)
	scanAt(200)
	write(t, filepath.Join(root, "calc.py"), "def add(a, b):", "    return a + b", "def neg(a):", "    return -a")
	scanAt(260)
	write(t, filepath.Join(root, "calc.py"), "def add(a, b):", "    return a + b", "def neg(a):", "    return -a", "def sub(a, b):", "    return a - b")
	scanAt(290)

	// Quiet for under ten minutes: a prompt may still claim the changes.
	if turns, _ := store.Turns("calcs", t0.Add(-time.Hour), t0.Add(time.Hour)); len(turns) != 1 {
		t.Fatalf("closed too early: %+v", turns)
	}
	scanAt(1000)

	turns, _ := store.Turns("calcs", t0.Add(-time.Hour), t0.Add(time.Hour))
	if len(turns) != 2 {
		t.Fatalf("want the prompt turn and one standalone turn, got %+v", turns)
	}
	st := turns[1]
	if st.Harness != standaloneHarness || st.EndReason != "standalone" || st.PromptLength != 0 || len(st.ChangedFiles) != 1 || st.ChangedFiles[0] != "calc.py" {
		t.Fatalf("standalone turn: %+v", st)
	}
	labels, _, _ := store.labelTurns(turns)
	l := labels[1]
	if l.Value != 1 || !l.NeedsReview || l.Coverage != "partial" || !contains(l.Signals, "human_edit_standalone") || l.ProductiveSeconds != 30 {
		t.Fatalf("standalone label: %+v", l)
	}
	if labels[0].ProductiveSeconds != 0 && contains(labels[0].Signals, "human_edit_standalone") {
		t.Fatalf("the prompt turn took the standalone edits: %+v", labels[0])
	}
	// The estimate counts the standalone seconds once.
	if human, _ := estimate(turns); human < 30 {
		t.Fatalf("estimate ignores standalone work: %d", human)
	}
}

// The same failing error twice is a repeat only when nothing new was
// learned: a second prompt that states a different hypothesis is effort, and a
// bare "again" is not.
func TestSameErrorDifferentHypothesisIsNotARepeat(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	transcript := filepath.Join(home, "projects", "-x", "s1.jsonl")
	err := "test_add fails with AssertionError -1 != 5 in calc.py"
	write(t, transcript,
		claudeLine("user", 0, cwd, `,"message":{"role":"user","content":"`+err+`"}`),
		claudeLine("assistant", 5, cwd, `,"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"tried a fix"}]}`),
		claudeLine("user", 200, cwd, `,"message":{"role":"user","content":"`+err+`. I think add() subtracts because the operator is wrong"}`),
		claudeLine("assistant", 205, cwd, `,"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"ok"}]}`),
		claudeLine("user", 400, cwd, `,"message":{"role":"user","content":"`+err+`"}`),
		claudeLine("assistant", 405, cwd, `,"message":{"model":"m","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	store, _ := openTestStore(t)
	src := []captureSource{{"claude-code", filepath.Join(home, "projects"), captureClaudeLine}}
	if _, e := store.CaptureScan(src, t0.Add(-time.Hour), t0.Add(500*time.Second)); e != nil {
		t.Fatal(e)
	}
	turns, _ := store.Turns("terminet", t0.Add(-time.Hour), t0.Add(time.Hour))
	if len(turns) != 3 {
		t.Fatalf("turns: %+v", turns)
	}
	labels, _, _ := store.labelTurns(turns)
	if !contains(labels[2].Signals, "near_repeat_of_earlier_prompt") || labels[2].Value != 0 || !labels[2].NeedsReview {
		t.Fatalf("identical third prompt should be a flagged repeat: %+v", labels[2])
	}
	if labels[1].Value != 1 || !contains(labels[1].Signals, "diagnosis") {
		t.Fatalf("a new hypothesis after the same error must count: %+v", labels[1])
	}
}
