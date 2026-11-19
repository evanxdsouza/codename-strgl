// SPDX-License-Identifier: AGPL-3.0-only

// strgl-collector tails Claude Code and Codex transcripts on this machine and
// sends WakaTime-compatible heartbeats, marked human or agent, to a Hackatime
// compatible server. Prompt and response text never leave the machine.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const version = "0.1.0"

// minGap is the shortest interval between two heartbeats of one session and
// actor. Human prompts are always sent.
const minGap = 30 * time.Second

type Heartbeat struct {
	Time           float64 `json:"time"`
	Entity         string  `json:"entity"`
	Type           string  `json:"type"`
	Category       string  `json:"category"`
	Project        string  `json:"project,omitempty"`
	Branch         string  `json:"branch,omitempty"`
	Actor          string  `json:"actor"`
	AIModel        string  `json:"ai_model,omitempty"`
	AISession      string  `json:"ai_session,omitempty"`
	AIPromptLength int     `json:"ai_prompt_length,omitempty"`
	AIInputTokens  int64   `json:"ai_input_tokens,omitempty"`
	AIOutputTokens int64   `json:"ai_output_tokens,omitempty"`
	UserAgent      string  `json:"user_agent"`
}

type fileState struct {
	Offset  int64       `json:"offset"`
	Context fileContext `json:"context"`
}

type State struct {
	Files    map[string]*fileState `json:"files"`
	LastSent map[string]time.Time  `json:"last_sent"` // session|actor
	Pending  map[string][2]int64   `json:"pending"`   // tokens of skipped agent events, by session
}

type source struct {
	harness string
	root    string
	parse   func([]byte, *fileContext) (Event, bool)
}

func main() {
	home, _ := os.UserHomeDir()
	once := flag.Bool("once", false, "scan once and exit")
	dryRun := flag.Bool("dry-run", false, "print the heartbeats that would be sent and exit, without sending or saving state")
	interval := flag.Duration("interval", time.Minute, "time between scans")
	backfill := flag.Duration("backfill", 7*24*time.Hour, "on first run, read transcript activity this far back")
	cfgPath := flag.String("config", filepath.Join(home, ".wakatime.cfg"), "WakaTime config file with api_url and api_key")
	statePath := flag.String("state", filepath.Join(home, ".strgl", "state.json"), "where read positions are kept")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	apiURL, apiKey := readConfig(*cfgPath)
	if !*dryRun && (apiURL == "" || apiKey == "") {
		log.Fatalf("set api_url and api_key in %s or STRGL_API_URL and STRGL_API_KEY", *cfgPath)
	}
	sources := defaultSources(home)

	for {
		state := loadState(*statePath)
		beats := scan(sources, state, time.Now().Add(-*backfill))
		switch {
		case *dryRun:
			out, _ := json.MarshalIndent(beats, "", "  ")
			fmt.Println(string(out))
			return
		case len(beats) == 0:
		default:
			if err := send(apiURL, apiKey, beats); err != nil {
				log.Printf("send failed, will retry: %v", err)
			} else if err := saveState(*statePath, state); err != nil {
				log.Printf("save state: %v", err)
			} else {
				log.Printf("sent %d heartbeats", len(beats))
			}
		}
		if len(beats) == 0 {
			_ = saveState(*statePath, state) // records newly seen files
		}
		if *once {
			return
		}
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		select {
		case <-stop:
			return
		case <-time.After(*interval):
		}
		signal.Stop(stop)
	}
}

func defaultSources(home string) []source {
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return []source{
		{harness: "claude-code", root: filepath.Join(claude, "projects"), parse: parseClaudeLine},
		{harness: "codex", root: filepath.Join(codex, "sessions"), parse: parseCodexLine},
	}
}

// scan reads every transcript line written since the last scan and returns
// the heartbeats to send. It updates state in place; the caller saves it only
// after the heartbeats were accepted, so a failed send is retried.
func scan(sources []source, state *State, since time.Time) []Heartbeat {
	var beats []Heartbeat
	for _, src := range sources {
		_ = filepath.WalkDir(src.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			fst, seen := state.Files[path]
			if !seen {
				fst = &fileState{}
				if info.ModTime().Before(since) {
					fst.Offset = info.Size() // old file: only read what is appended later
				}
				state.Files[path] = fst
			}
			if info.Size() < fst.Offset {
				fst.Offset = 0 // rewritten
			}
			if info.Size() == fst.Offset {
				return nil
			}
			beats = append(beats, readNew(src, path, fst, state, since)...)
			return nil
		})
	}
	return beats
}

func readNew(src source, path string, fst *fileState, state *State, since time.Time) []Heartbeat {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(fst.Offset, io.SeekStart); err != nil {
		return nil
	}
	reader := bufio.NewReaderSize(f, 1<<20)
	var beats []Heartbeat
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break // a partial last line is read again next scan
		}
		fst.Offset += int64(len(line))
		ev, ok := src.parse(bytes.TrimSpace(line), &fst.Context)
		if !ok || ev.Time.Before(since) {
			continue
		}
		if hb, ok := toHeartbeat(src.harness, ev, state); ok {
			beats = append(beats, hb)
		}
	}
	return beats
}

func toHeartbeat(harness string, ev Event, state *State) (Heartbeat, bool) {
	key := ev.Session + "|" + ev.Actor
	if ev.Actor == "agent" {
		pending := state.Pending[ev.Session]
		pending[0] += ev.InputTokens
		pending[1] += ev.OutputTokens
		state.Pending[ev.Session] = pending
		if last, ok := state.LastSent[key]; ok && ev.Time.Sub(last) < minGap && ev.Time.After(last) {
			return Heartbeat{}, false
		}
	}
	state.LastSent[key] = ev.Time
	project := projectFor(ev.Cwd)
	hb := Heartbeat{
		Time:           float64(ev.Time.UnixNano()) / 1e9,
		Entity:         harness,
		Type:           "app",
		Category:       "ai coding",
		Project:        project.Name,
		Branch:         project.Branch,
		Actor:          ev.Actor,
		AIModel:        ev.Model,
		AISession:      ev.Session,
		AIPromptLength: ev.PromptLength,
		UserAgent:      userAgent(harness, ev.Version),
	}
	if ev.Actor == "agent" {
		pending := state.Pending[ev.Session]
		hb.AIInputTokens, hb.AIOutputTokens = pending[0], pending[1]
		delete(state.Pending, ev.Session)
	}
	return hb, true
}

// userAgent puts the harness before the collector so the server's user-agent
// parser reports the harness as the editor.
func userAgent(harness, harnessVersion string) string {
	if harnessVersion == "" {
		harnessVersion = "unknown"
	}
	return fmt.Sprintf("wakatime/v%s (%s-%s) %s %s/%s strgl-collector/%s",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version(), harness, harnessVersion, version)
}

func send(apiURL, apiKey string, beats []Heartbeat) error {
	hostname, _ := os.Hostname()
	client := &http.Client{Timeout: 30 * time.Second}
	for start := 0; start < len(beats); start += 100 {
		batch := beats[start:min(start+100, len(beats))]
		body, _ := json.Marshal(batch)
		req, err := http.NewRequest("POST", strings.TrimRight(apiURL, "/")+"/users/current/heartbeats.bulk", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "strgl-collector/"+version)
		req.Header.Set("X-Machine-Name", hostname)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("server returned %s", resp.Status)
		}
	}
	return nil
}

// readConfig reads api_url and api_key from a WakaTime-style INI file, with
// STRGL_API_URL and STRGL_API_KEY taking precedence.
func readConfig(path string) (apiURL, apiKey string) {
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch strings.TrimSpace(key) {
			case "api_url":
				apiURL = strings.TrimSpace(value)
			case "api_key":
				apiKey = strings.TrimSpace(value)
			}
		}
	}
	if v := os.Getenv("STRGL_API_URL"); v != "" {
		apiURL = v
	}
	if v := os.Getenv("STRGL_API_KEY"); v != "" {
		apiKey = v
	}
	return apiURL, apiKey
}

func loadState(path string) *State {
	state := &State{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, state)
	}
	if state.Files == nil {
		state.Files = map[string]*fileState{}
	}
	if state.LastSent == nil {
		state.LastSent = map[string]time.Time{}
	}
	if state.Pending == nil {
		state.Pending = map[string][2]int64{}
	}
	return state
}

func saveState(path string, state *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
