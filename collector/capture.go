// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Capture is one transcript line read for the local turn timeline. Unlike
// Event it may hold raw text, so it is only ever written, encrypted, to the
// local evidence store and never sent anywhere.
type Capture struct {
	Time         time.Time
	Session      string
	Parent       string // the parent session of a child agent session, when known
	Cwd          string
	Model        string
	Version      string
	Human        bool // a genuine human prompt starts a new turn
	PromptLength int
	InputTokens  int64
	OutputTokens int64
	End          bool // the agent finished, failed or was interrupted
	Interrupted  bool
	Items        []Item
}

// Item is one piece of local evidence attached to a turn.
type Item struct {
	Kind    string `json:"kind"` // prompt, response, tool_call, tool_output, diff
	Text    string `json:"text,omitempty"`
	Tool    string `json:"tool,omitempty"`
	ToolID  string `json:"tool_id,omitempty"`
	Command string `json:"command,omitempty"` // shell command of a tool call
	Exit    *int   `json:"exit,omitempty"`    // observed exit code of a tool output
	IsError bool   `json:"is_error,omitempty"`
}

type captureSource struct {
	harness string
	root    string
	parse   func([]byte, *fileContext) (Capture, bool)
}

func defaultCaptureSources(home string) []captureSource {
	var out []captureSource
	for _, src := range defaultSources(home) {
		switch src.harness {
		case "claude-code":
			out = append(out, captureSource{src.harness, src.root, captureClaudeLine})
		case "codex":
			out = append(out, captureSource{src.harness, src.root, captureCodexLine})
		}
	}
	dir := filepath.Join(home, ".strgl", "opencode-evidence")
	if v := os.Getenv("STRGL_OPENCODE_DIR"); v != "" {
		dir = filepath.Join(v, "evidence")
	}
	dir = envOr("STRGL_OPENCODE_EVIDENCE_DIR", dir)
	return append(out, captureSource{"opencode", dir, captureOpenCodeLine})
}

func fromEvent(ev Event) Capture {
	return Capture{
		Time: ev.Time, Session: ev.Session, Cwd: ev.Cwd, Model: ev.Model, Version: ev.Version,
		Human: ev.Actor == "human", PromptLength: ev.PromptLength,
		InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens, End: ev.End,
	}
}

// captureClaudeLine classifies a line with parseClaudeLine, so prompts are
// counted the same way as for heartbeats, then keeps its text locally.
func captureClaudeLine(line []byte, ctx *fileContext) (Capture, bool) {
	ev, ok := parseClaudeLine(line, ctx)
	if !ok {
		return Capture{}, false
	}
	c := fromEvent(ev)
	var rec struct {
		Type          string `json:"type"`
		ToolUseResult struct {
			Interrupted bool `json:"interrupted"`
		} `json:"toolUseResult"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal(line, &rec)
	var text string
	if json.Unmarshal(rec.Message.Content, &text) == nil {
		if c.Human {
			c.Items = append(c.Items, Item{Kind: "prompt", Text: text})
		} else if strings.HasPrefix(strings.TrimSpace(text), "[Request interrupted") {
			c.Interrupted, c.End = true, true
		}
		return c, true
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	_ = json.Unmarshal(rec.Message.Content, &blocks)
	for _, b := range blocks {
		switch {
		case b.Type == "text" && c.Human && !isInjected(b.Text):
			c.Items = append(c.Items, Item{Kind: "prompt", Text: b.Text})
		case b.Type == "text" && strings.HasPrefix(strings.TrimSpace(b.Text), "[Request interrupted"):
			c.Interrupted, c.End = true, true
		case b.Type == "text" && rec.Type == "assistant":
			c.Items = append(c.Items, Item{Kind: "response", Text: b.Text})
		case b.Type == "tool_use":
			c.Items = append(c.Items, Item{Kind: "tool_call", Tool: b.Name, ToolID: b.ID, Text: string(b.Input), Command: jsonField(b.Input, "command")})
		case b.Type == "tool_result":
			// Claude Code reports a non-zero exit as is_error; it does not
			// record the exit code itself.
			c.Items = append(c.Items, Item{Kind: "tool_output", ToolID: b.ToolUseID, Text: blockText(b.Content), IsError: b.IsError})
		}
	}
	if rec.ToolUseResult.Interrupted {
		c.Interrupted = true
	}
	return c, true
}

// captureCodexLine keeps prompts, agent messages, tool calls and command
// results of a Codex rollout, classified like parseCodexLine.
func captureCodexLine(line []byte, ctx *fileContext) (Capture, bool) {
	var rec struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return Capture{}, false
	}
	var p struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Message   string          `json:"message"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
		Command   json.RawMessage `json:"command"`
		ExitCode  *int            `json:"exit_code"`
		Aggregate string          `json:"aggregated_output"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Item struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	_ = json.Unmarshal(rec.Payload, &p)
	ev, ok := parseCodexLine(line, ctx)
	var c Capture
	if ok {
		c = fromEvent(ev)
	} else {
		at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
		if err != nil || rec.Type != "event_msg" || (p.Type != "exec_command_end" && p.Type != "turn_aborted") {
			return Capture{}, false
		}
		c = Capture{Time: at, Session: ctx.Session, Cwd: ctx.Cwd, Model: ctx.Model, Version: ctx.Version}
	}
	switch {
	case c.Human && p.Type == "user_message":
		c.Items = append(c.Items, Item{Kind: "prompt", Text: p.Message})
	case c.Human:
		var parts []string
		for _, part := range p.Item.Content {
			parts = append(parts, part.Text)
		}
		c.Items = append(c.Items, Item{Kind: "prompt", Text: strings.Join(parts, "\n")})
	case rec.Type == "event_msg" && p.Type == "agent_message":
		c.Items = append(c.Items, Item{Kind: "response", Text: p.Message})
	case rec.Type == "event_msg" && p.Type == "exec_command_end":
		c.Items = append(c.Items, Item{Kind: "tool_output", Tool: "exec_command", ToolID: p.CallID, Command: commandString(p.Command), Text: p.Aggregate, Exit: p.ExitCode, IsError: p.ExitCode != nil && *p.ExitCode != 0})
	case rec.Type == "event_msg" && p.Type == "turn_aborted":
		c.Interrupted, c.End = true, true
	case rec.Type == "event_msg" && (p.Type == "task_complete" || p.Type == "turn_complete"):
		c.End = true
	case rec.Type == "response_item" && p.Type == "function_call":
		c.Items = append(c.Items, Item{Kind: "tool_call", Tool: p.Name, ToolID: p.CallID, Text: p.Arguments, Command: jsonField(json.RawMessage(p.Arguments), "command")})
	}
	return c, true
}

// captureOpenCodeLine reads the evidence spool that opencode.js writes when
// evidence capture is on. It is the only OpenCode file with text.
func captureOpenCodeLine(line []byte, ctx *fileContext) (Capture, bool) {
	var rec struct {
		Schema      int             `json:"schema"`
		Kind        string          `json:"kind"`
		Sequence    int64           `json:"sequence"`
		Timestamp   int64           `json:"timestamp"`
		Session     string          `json:"session_id"`
		Parent      string          `json:"parent_id"`
		Cwd         string          `json:"cwd"`
		Model       string          `json:"model"`
		Version     string          `json:"version"`
		Text        string          `json:"text"`
		Tool        string          `json:"tool"`
		ToolID      string          `json:"tool_id"`
		Input       json.RawMessage `json:"input"`
		Exit        *int            `json:"exit"`
		Failed      bool            `json:"failed"`
		Interrupted bool            `json:"interrupted"`
		Input_      int64           `json:"input_tokens"`
		Output      int64           `json:"output_tokens"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Schema != 1 || rec.Timestamp <= 0 || rec.Session == "" {
		return Capture{}, false
	}
	if rec.Sequence > 0 {
		if rec.Sequence <= ctx.OpenCodeSeq {
			return Capture{}, false
		}
		ctx.OpenCodeSeq = rec.Sequence
	}
	ctx.Cwd, ctx.Model, ctx.Version = firstNonEmpty(rec.Cwd, ctx.Cwd), firstNonEmpty(rec.Model, ctx.Model), firstNonEmpty(rec.Version, ctx.Version)
	c := Capture{
		Time: time.UnixMilli(rec.Timestamp).UTC(), Session: rec.Session, Parent: rec.Parent,
		Cwd: ctx.Cwd, Model: ctx.Model, Version: ctx.Version,
		InputTokens: rec.Input_, OutputTokens: rec.Output,
	}
	switch rec.Kind {
	case "prompt":
		c.Human, c.PromptLength = true, len([]rune(rec.Text))
		c.Items = []Item{{Kind: "prompt", Text: rec.Text}}
	case "response":
		c.Items = []Item{{Kind: "response", Text: rec.Text}}
	case "tool_call":
		c.Items = []Item{{Kind: "tool_call", Tool: rec.Tool, ToolID: rec.ToolID, Text: string(rec.Input), Command: jsonField(rec.Input, "command")}}
	case "tool_output":
		c.Items = []Item{{Kind: "tool_output", Tool: rec.Tool, ToolID: rec.ToolID, Text: rec.Text, Exit: rec.Exit, IsError: rec.Failed || (rec.Exit != nil && *rec.Exit != 0)}}
	case "end":
		c.End, c.Interrupted = true, rec.Interrupted
	case "agent":
	default:
		return Capture{}, false
	}
	return c, true
}

func jsonField(raw json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return commandString(m[key])
}

// commandString accepts a command as a string or an argv array.
func commandString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var argv []string
	if json.Unmarshal(raw, &argv) == nil {
		return strings.Join(argv, " ")
	}
	return ""
}

// blockText flattens tool_result content, a string or text blocks.
func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
