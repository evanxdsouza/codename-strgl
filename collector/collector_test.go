// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secretPrompt = "please fix my secret-token-123 bug"

var t0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func ts(seconds int) string {
	return t0.Add(time.Duration(seconds) * time.Second).Format(time.RFC3339Nano)
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A git repository with a subdirectory, as an agent's working directory.
func repo(t *testing.T) (root, sub string) {
	root = filepath.Join(t.TempDir(), "terminet")
	sub = filepath.Join(root, "src", "ui")
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/feature-x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, sub
}

func claudeLine(kind string, seconds int, cwd, extra string) string {
	return fmt.Sprintf(`{"type":%q,"timestamp":%q,"cwd":%q,"sessionId":"s1","version":"2.1.289","isSidechain":false%s}`, kind, ts(seconds), cwd, extra)
}

func scanOnce(t *testing.T, src source, state *State) []Heartbeat {
	t.Helper()
	return scan([]source{src}, state, t0.Add(-time.Hour))
}

func TestClaudeLongAgentRunWithShortHumanPrompt(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	lines := []string{
		`{"type":"permission-mode","permissionMode":"default","sessionId":"s1"}`,
		claudeLine("user", 0, cwd, `,"message":{"role":"user","content":"`+secretPrompt+`"}`),
	}
	// Ten minutes of agent work: an assistant message every 10 seconds and a
	// tool result after each.
	for s := 5; s <= 600; s += 10 {
		lines = append(lines,
			claudeLine("assistant", s, cwd, `,"message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":100,"output_tokens":10}}`),
			claudeLine("user", s+1, cwd, `,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}`))
	}
	lines = append(lines, claudeLine("user", 20, cwd, `,"isMeta":true,"message":{"role":"user","content":"<local-command-caveat>x"}`))
	write(t, filepath.Join(home, "projects", "-x", "s1.jsonl"), lines...)

	state := loadState(filepath.Join(home, "state.json"))
	beats := scanOnce(t, source{"claude-code", filepath.Join(home, "projects"), parseClaudeLine}, state)

	humans, agents := 0, 0
	var inTokens int64
	for _, b := range beats {
		if b.Project != "terminet" || b.Branch != "feature-x" || b.Entity != "claude-code" {
			t.Fatalf("wrong project or entity: %+v", b)
		}
		switch b.Actor {
		case "human":
			humans++
			if b.AIPromptLength != len(secretPrompt) {
				t.Errorf("prompt length %d, want %d", b.AIPromptLength, len(secretPrompt))
			}
		case "agent":
			agents++
			inTokens += b.AIInputTokens
		}
	}
	if humans != 1 {
		t.Errorf("human heartbeats = %d, want 1 (tool results and meta lines are not prompts)", humans)
	}
	// 120 agent lines over ~600s, at most one per 30s, plus the boundary at 2 minutes.
	if agents < 18 || agents > 23 {
		t.Errorf("agent heartbeats = %d, want about 20", agents)
	}
	// Tokens of skipped events roll into the next sent heartbeat; only the
	// tail after the last sent one is still pending.
	if inTokens < 5000 || inTokens > 6000 {
		t.Errorf("input tokens sent = %d, want most of 6000", inTokens)
	}

	out, _ := json.Marshal(beats)
	if strings.Contains(string(out), "secret-token") || strings.Contains(string(out), `"ls"`) {
		t.Fatal("prompt or tool text leaked into heartbeats")
	}
	if !strings.Contains(beats[0].UserAgent, "claude-code/2.1.289 strgl-collector/") {
		t.Errorf("user agent %q", beats[0].UserAgent)
	}

	// A second scan reads nothing new; an appended prompt is picked up.
	if again := scanOnce(t, source{"claude-code", filepath.Join(home, "projects"), parseClaudeLine}, state); len(again) != 0 {
		t.Fatalf("rescan sent %d heartbeats again", len(again))
	}
	f, _ := os.OpenFile(filepath.Join(home, "projects", "-x", "s1.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f, claudeLine("user", 700, cwd, `,"message":{"role":"user","content":[{"type":"text","text":"next"}]}`))
	fmt.Fprint(f, `{"type":"user","timestamp":"partial`) // half-written line
	f.Close()
	more := scanOnce(t, source{"claude-code", filepath.Join(home, "projects"), parseClaudeLine}, state)
	if len(more) != 1 || more[0].Actor != "human" || more[0].AIPromptLength != 4 {
		t.Fatalf("appended prompt: got %+v", more)
	}
}

func TestCodexLegacyAndPaginatedPrompts(t *testing.T) {
	root, cwd := repo(t)
	home := t.TempDir()
	write(t, filepath.Join(home, "sessions", "2026", "11", "18", "rollout-2026-11-18T10-00-00-abc.jsonl"),
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"abc","timestamp":%q,"cwd":%q,"originator":"codex_cli_rs","cli_version":"0.116.0"}}`, ts(0), ts(0), root),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"cwd":%q,"model":"gpt-6-sol"}}`, ts(0), cwd),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":%q,"kind":"plain"}}`, ts(1), secretPrompt),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, ts(1), secretPrompt),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}`, ts(5)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":900},"last_token_usage":{"input_tokens":300,"output_tokens":40}}}}`, ts(50)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"item_completed","thread_id":"abc","turn_id":"t2","item":{"type":"UserMessage","id":"u2","content":[{"type":"text","text":"again"}]}}}`, ts(90)),
	)

	beats := scanOnce(t, source{"codex", filepath.Join(home, "sessions"), parseCodexLine}, loadState(filepath.Join(home, "state.json")))

	var got []string
	for _, b := range beats {
		got = append(got, fmt.Sprintf("%s:%d:%d", b.Actor, b.AIPromptLength, b.AIInputTokens))
		if b.Project != "terminet" || b.AIModel != "gpt-6-sol" || b.AISession != "abc" {
			t.Fatalf("wrong metadata: %+v", b)
		}
	}
	want := "human:34:0 agent:0:0 agent:0:300 human:5:0"
	if strings.Join(got, " ") != want {
		t.Errorf("heartbeats = %q, want %q (the user-role response item repeats the prompt and must not count)", strings.Join(got, " "), want)
	}
	out, _ := json.Marshal(beats)
	if strings.Contains(string(out), "secret-token") {
		t.Fatal("prompt text leaked into heartbeats")
	}
}

func TestOldFilesAreNotBackfilled(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	path := filepath.Join(home, "projects", "-x", "old.jsonl")
	write(t, path, claudeLine("user", 0, cwd, `,"message":{"role":"user","content":"hi"}`))
	old := t0.Add(-48 * time.Hour)
	_ = os.Chtimes(path, old, old)

	beats := scan([]source{{"claude-code", filepath.Join(home, "projects"), parseClaudeLine}}, loadState(filepath.Join(home, "s.json")), t0.Add(-24*time.Hour))
	if len(beats) != 0 {
		t.Fatalf("old file sent %d heartbeats", len(beats))
	}
}

func TestProjectWithoutGitUsesDirectoryName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scratch")
	_ = os.MkdirAll(dir, 0o755)
	if p := projectFor(dir); p.Name != "scratch" || p.Branch != "" {
		t.Fatalf("got %+v", p)
	}
}

func TestAgentHeartbeatMarksTheEndOfTheCreditWindow(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	lines := []string{claudeLine("user", 0, cwd, `,"message":{"role":"user","content":"go"}`)}
	// Agent output every 25 seconds for 10 minutes: none lands on 120 exactly.
	for s := 25; s <= 600; s += 25 {
		lines = append(lines, claudeLine("assistant", s, cwd, `,"message":{"model":"m","role":"assistant","content":[]}`))
	}
	write(t, filepath.Join(home, "projects", "-x", "s1.jsonl"), lines...)

	beats := scanOnce(t, source{"claude-code", filepath.Join(home, "projects"), parseClaudeLine}, loadState(filepath.Join(home, "s.json")))

	start := float64(t0.Unix())
	boundaries := 0
	for i, b := range beats {
		if i > 0 && b.Time < beats[i-1].Time {
			t.Fatalf("heartbeats out of order at %d", i)
		}
		if b.Time == start+120 {
			boundaries++
			if b.Actor != "agent" {
				t.Errorf("boundary heartbeat actor %q", b.Actor)
			}
		}
	}
	if boundaries != 1 {
		t.Fatalf("heartbeats at prompt + 120s = %d, want 1", boundaries)
	}
}

func TestNoBoundaryWhenTheAgentStopsInsideTheWindow(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	write(t, filepath.Join(home, "projects", "-x", "s1.jsonl"),
		claudeLine("user", 0, cwd, `,"message":{"role":"user","content":"go"}`),
		claudeLine("assistant", 45, cwd, `,"message":{"model":"m","role":"assistant","content":[]}`),
		claudeLine("user", 500, cwd, `,"message":{"role":"user","content":"next"}`))

	beats := scanOnce(t, source{"claude-code", filepath.Join(home, "projects"), parseClaudeLine}, loadState(filepath.Join(home, "s.json")))
	if len(beats) != 3 {
		t.Fatalf("got %d heartbeats, want 3 (no boundary after the agent stopped at 45s)", len(beats))
	}
}

func TestOnlyTypedPromptsAreHuman(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	summary := "This session is being continued from a previous conversation that ran out of context. The conversation is summarized below:\\nAnalysis:\\n..."
	write(t, filepath.Join(home, "projects", "-x", "s1.jsonl"),
		claudeLine("user", 0, cwd, `,"message":{"role":"user","content":"real prompt"}`),
		`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","isMeta":false,"timestamp":"`+ts(1)+`","compactMetadata":{"trigger":"auto","preTokens":156579}}`,
		// Compaction summary with its flag, and one without it (text fallback).
		claudeLine("user", 1, cwd, `,"message":{"role":"user","content":"`+summary+`"},"isVisibleInTranscriptOnly":true,"isCompactSummary":true`),
		claudeLine("user", 2, cwd, `,"message":{"role":"user","content":"`+summary+`"}`),
		claudeLine("user", 3, cwd, `,"origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>done</task-notification>"}`),
		claudeLine("user", 4, cwd, `,"origin":{"kind":"peer"},"message":{"role":"user","content":"hello from another session"}`),
		claudeLine("user", 5, cwd, `,"message":{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"}]}`),
		claudeLine("user", 6, cwd, `,"message":{"role":"user","content":"<bash-stdout>ok</bash-stdout><bash-stderr></bash-stderr>"}`),
		claudeLine("user", 7, cwd, `,"message":{"role":"user","content":"<command-name>/clear</command-name>"}`),
		claudeLine("user", 8, cwd, `,"origin":{"kind":"human"},"message":{"role":"user","content":[{"type":"text","text":"second prompt"},{"type":"text","text":"<system-reminder>ctx</system-reminder>"}]}`),
	)

	beats := scanOnce(t, source{"claude-code", filepath.Join(home, "projects"), parseClaudeLine}, loadState(filepath.Join(home, "s.json")))
	var humans []int
	for _, b := range beats {
		if b.Actor == "human" {
			humans = append(humans, b.AIPromptLength)
		}
	}
	if fmt.Sprint(humans) != "[11 13]" {
		t.Fatalf("human prompt lengths = %v, want [11 13] (only the two typed prompts)", humans)
	}
}

func TestOpenCodeCompletionAndReplay(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	t.Setenv("STRGL_OPENCODE_DIR", filepath.Join(home, "opencode"))
	path := filepath.Join(home, "opencode", "ses_test.jsonl")
	line := func(actor string, seconds, sequence int, extra string) string {
		return fmt.Sprintf(`{"schema":1,"timestamp":%d,"actor":%q,"session_id":"ses_test","cwd":%q,"model":"local/custom-model","version":"2.3.2","sequence":%d%s}`,
			t0.Add(time.Duration(seconds)*time.Second).UnixMilli(), actor, cwd, sequence, extra)
	}
	write(t, path,
		line("human", 0, 2, `,"prompt_length":5`),
		line("agent", 5, 3, ""),
		line("agent", 45, 4, `,"input_tokens":103,"output_tokens":17`),
		line("agent", 59, 5, `,"input_tokens":205,"output_tokens":31`),
		line("agent", 61, 6, `,"end":true`))

	statePath := filepath.Join(home, "state.json")
	state := loadState(statePath)
	beats := scan(defaultSources(home), state, t0.Add(-time.Hour))
	var got []string
	for _, b := range beats {
		got = append(got, fmt.Sprintf("%s:%.0f:%d:%d", b.Actor, b.Time-float64(t0.Unix()), b.AIInputTokens, b.AIOutputTokens))
		if b.Entity != "opencode" || b.Project != "terminet" || b.Branch != "feature-x" || b.AIModel != "local/custom-model" || b.AISession != "ses_test" {
			t.Fatalf("wrong OpenCode metadata: %+v", b)
		}
		if !strings.Contains(b.UserAgent, "opencode/2.3.2 strgl-collector/") {
			t.Fatalf("wrong user agent: %s", b.UserAgent)
		}
	}
	if want := "human:0:0:0 agent:5:0:0 agent:45:103:17 agent:61:205:31"; strings.Join(got, " ") != want {
		t.Fatalf("heartbeats = %v, want %s (completion must flush the tail and its tokens)", got, want)
	}
	if err := saveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Replayed durable events are not new prompts, even after the collector
	// reloads its state. A later autonomous event must not fabricate a 120s
	// credit boundary for the run that already ended at 61s.
	fmt.Fprintln(f, line("human", 0, 2, `,"prompt_length":5`))
	fmt.Fprintln(f, line("agent", 61, 6, `,"end":true`))
	fmt.Fprintln(f, line("agent", 400, 7, ""))
	f.Close()
	more := scan(defaultSources(home), loadState(statePath), t0.Add(-time.Hour))
	if len(more) != 1 || more[0].Actor != "agent" || more[0].Time != float64(t0.Unix())+400 {
		t.Fatalf("replay/autonomous continuation = %+v, want one agent heartbeat at 400s", more)
	}
}

func TestOpenCodeLongRunKeepsTheTwoMinuteWindow(t *testing.T) {
	_, cwd := repo(t)
	home := t.TempDir()
	line := func(actor string, seconds, sequence int, extra string) string {
		return fmt.Sprintf(`{"schema":1,"timestamp":%d,"actor":%q,"session_id":"ses_long","cwd":%q,"sequence":%d%s}`,
			t0.Add(time.Duration(seconds)*time.Second).UnixMilli(), actor, cwd, sequence, extra)
	}
	lines := []string{line("human", 0, 1, `,"prompt_length":2`)}
	for s := 25; s <= 600; s += 25 {
		lines = append(lines, line("agent", s, s/25+1, `,"input_tokens":7,"output_tokens":3`))
	}
	lines = append(lines, line("agent", 601, 26, `,"end":true`))
	write(t, filepath.Join(home, "ses_long.jsonl"), lines...)
	beats := scanOnce(t, source{"opencode", home, parseOpenCodeLine}, loadState(filepath.Join(home, "state.json")))
	boundaries := 0
	var raw float64
	var input, output int64
	for i, b := range beats {
		input += b.AIInputTokens
		output += b.AIOutputTokens
		if b.Time == float64(t0.Unix())+120 && b.Actor == "agent" {
			boundaries++
		}
		if i > 0 {
			raw += min(b.Time-beats[i-1].Time, 120)
		}
	}
	if boundaries != 1 || raw != 601 || input != 168 || output != 72 {
		t.Fatalf("boundary count/raw seconds/tokens = %d/%.0f/%d/%d, want 1/601/168/72", boundaries, raw, input, output)
	}
}
