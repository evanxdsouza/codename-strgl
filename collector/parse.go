// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Event is one transcript line reduced to metadata. It never holds prompt or
// response text: only the prompt's length in characters.
type Event struct {
	Time         time.Time
	Actor        string // "human" or "agent"
	Cwd          string
	Session      string
	Model        string
	Version      string // harness version
	PromptLength int    // > 0 only for a human prompt
	InputTokens  int64
	OutputTokens int64
}

// fileContext carries what a transcript states once and later lines rely on,
// such as Codex's session_meta cwd.
type fileContext struct {
	Cwd     string
	Session string
	Model   string
	Version string
}

// parseClaudeLine reads one line of a Claude Code transcript
// (~/.claude/projects/<key>/<session>.jsonl). A user line whose content is
// typed text is a human prompt; assistant lines, tool results and subagent
// (sidechain) lines are agent activity. Bookkeeping lines are skipped.
func parseClaudeLine(line []byte, ctx *fileContext) (Event, bool) {
	var rec struct {
		Type        string `json:"type"`
		Timestamp   string `json:"timestamp"`
		Cwd         string `json:"cwd"`
		SessionID   string `json:"sessionId"`
		Version     string `json:"version"`
		IsSidechain bool   `json:"isSidechain"`
		IsMeta      bool   `json:"isMeta"`
		Message     struct {
			Model   string          `json:"model"`
			Content json.RawMessage `json:"content"`
			Usage   struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || (rec.Type != "user" && rec.Type != "assistant") {
		return Event{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return Event{}, false
	}
	if rec.Cwd != "" {
		ctx.Cwd = rec.Cwd
	}
	if rec.SessionID != "" {
		ctx.Session = rec.SessionID
	}
	if rec.Version != "" {
		ctx.Version = rec.Version
	}
	ev := Event{Time: at, Actor: "agent", Cwd: ctx.Cwd, Session: ctx.Session, Version: ctx.Version}

	if rec.Type == "assistant" {
		if rec.Message.Model != "" {
			ctx.Model = rec.Message.Model
		}
		ev.Model = ctx.Model
		ev.InputTokens = rec.Message.Usage.InputTokens
		ev.OutputTokens = rec.Message.Usage.OutputTokens
		return ev, true
	}

	if rec.IsMeta {
		return Event{}, false
	}
	if n := claudePromptLength(rec.Message.Content); n > 0 && !rec.IsSidechain {
		ev.Actor = "human"
		ev.PromptLength = n
	}
	return ev, true
}

// claudePromptLength is the typed text length of a user message: a plain
// string, or text blocks of an array. An array of tool results is 0.
func claudePromptLength(content json.RawMessage) int {
	var text string
	if json.Unmarshal(content, &text) == nil {
		if isInjected(text) {
			return 0
		}
		return utf8.RuneCountInString(text)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return 0
	}
	n := 0
	for _, b := range blocks {
		if b.Type == "tool_result" {
			return 0
		}
		if b.Type == "text" && !isInjected(b.Text) {
			n += utf8.RuneCountInString(b.Text)
		}
	}
	return n
}

// isInjected reports text Claude Code writes into a user turn itself, such as
// slash command and local command output wrappers.
func isInjected(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<local-command-") || strings.HasPrefix(t, "<command-name>") ||
		strings.HasPrefix(t, "<command-message>") || strings.HasPrefix(t, "[Request interrupted")
}

// parseCodexLine reads one line of a Codex rollout
// (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl). Prompts arrive as an
// event_msg user_message (legacy history) or an item_completed UserMessage
// (paginated history); model output, tool calls and token counts are agent
// activity. User-role response items repeat the prompt plus injected
// instructions, so they are skipped.
func parseCodexLine(line []byte, ctx *fileContext) (Event, bool) {
	var rec struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return Event{}, false
	}
	var p struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		Cwd        string `json:"cwd"`
		Model      string `json:"model"`
		CliVersion string `json:"cli_version"`
		Role       string `json:"role"`
		Message    string `json:"message"`
		Item       struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
		Info *struct {
			Last struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"last_token_usage"`
		} `json:"info"`
	}
	_ = json.Unmarshal(rec.Payload, &p)

	switch rec.Type {
	case "session_meta":
		ctx.Cwd, ctx.Session, ctx.Version = firstNonEmpty(p.Cwd, ctx.Cwd), firstNonEmpty(p.ID, ctx.Session), firstNonEmpty(p.CliVersion, ctx.Version)
		return Event{}, false
	case "turn_context":
		ctx.Cwd, ctx.Model = firstNonEmpty(p.Cwd, ctx.Cwd), firstNonEmpty(p.Model, ctx.Model)
		return Event{}, false
	}

	at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return Event{}, false
	}
	ev := Event{Time: at, Actor: "agent", Cwd: ctx.Cwd, Session: ctx.Session, Model: ctx.Model, Version: ctx.Version}

	switch {
	case rec.Type == "event_msg" && p.Type == "user_message":
		ev.Actor, ev.PromptLength = "human", max(1, utf8.RuneCountInString(p.Message))
	case rec.Type == "event_msg" && p.Type == "item_completed" && p.Item.Type == "UserMessage":
		n := 0
		for _, c := range p.Item.Content {
			n += utf8.RuneCountInString(c.Text)
		}
		ev.Actor, ev.PromptLength = "human", max(1, n)
	case rec.Type == "event_msg" && p.Type == "token_count" && p.Info != nil:
		ev.InputTokens, ev.OutputTokens = p.Info.Last.InputTokens, p.Info.Last.OutputTokens
	case rec.Type == "response_item" && p.Role != "user":
	case rec.Type == "event_msg" && (p.Type == "agent_message" || p.Type == "task_started" || p.Type == "turn_started" || p.Type == "task_complete" || p.Type == "turn_complete"):
	default:
		return Event{}, false
	}
	return ev, true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
