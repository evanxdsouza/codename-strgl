// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// NERHelper runs an optional, user-installed named-entity model as a
// separate process, so the collector stays one small binary and heavier
// inference never runs unless the user asks for it. The protocol is one JSON
// object per line: the helper first prints {"model": "<id>"}, then answers
// each {"text": "..."} with {"entities": [{"type": "PERSON", "text": "..."}]}.
// Entities are returned as text, not offsets, so byte and character offsets
// cannot disagree, and every occurrence of a name is removed.
type NERHelper struct {
	Model string

	mu   sync.Mutex
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	dead error
}

type nerEntity struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// StartNER starts the helper command line (split on spaces) and reads its
// model line. Any failure is returned, and the caller must not build a
// bundle without it: when the user enabled the model, redaction fails closed.
func StartNER(command string) (*NERHelper, error) {
	args := strings.Fields(command)
	if len(args) == 0 {
		return nil, fmt.Errorf("empty NER helper command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start NER helper: %w", err)
	}
	h := &NERHelper{cmd: cmd, in: in, out: bufio.NewReaderSize(stdout, 1<<20)}
	var hello struct {
		Model string `json:"model"`
	}
	if err := h.readLine(&hello); err != nil || hello.Model == "" {
		h.Close()
		return nil, fmt.Errorf("NER helper did not report its model: %v", err)
	}
	h.Model = hello.Model
	return h, nil
}

func (h *NERHelper) readLine(v any) error {
	line, err := h.out.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Entities asks the model for the names, places and organisations in text.
// After one failure the helper is treated as dead, so later items are
// withheld rather than sent with only rule-based redaction.
func (h *NERHelper) Entities(text string) ([]nerEntity, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dead != nil {
		return nil, h.dead
	}
	req, _ := json.Marshal(map[string]string{"text": text})
	var resp struct {
		Entities []nerEntity `json:"entities"`
		Error    string      `json:"error"`
	}
	_, err := h.in.Write(append(req, '\n'))
	if err == nil {
		err = h.readLine(&resp)
	}
	if err == nil && resp.Error != "" {
		err = fmt.Errorf("%s", resp.Error)
	}
	if err != nil {
		h.dead = fmt.Errorf("NER helper failed: %w", err)
		return nil, h.dead
	}
	return resp.Entities, nil
}

func (h *NERHelper) Close() {
	if h == nil || h.cmd == nil {
		return
	}
	h.in.Close()
	_ = h.cmd.Wait()
}
