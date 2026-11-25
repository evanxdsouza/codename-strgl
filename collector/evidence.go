// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// serverBase turns the heartbeat api_url (…/api/hackatime/v1) into the
// server's root URL, for the struggle tracker's own endpoints.
func serverBase(apiURL string) string {
	base := strings.TrimRight(apiURL, "/")
	for _, suffix := range []string{"/api/hackatime/v1", "/api/v1"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base
}

func postJSON(url, apiKey string, body []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "strgl-collector/"+version)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// anchor sends the newest unanchored chain head: a hash only, no content.
// The server's receipt time bounds when the evidence before it existed.
func (s *Store) anchor(apiURL, apiKey string, now time.Time) error {
	seq, head, ok := s.unanchoredHead()
	if !ok {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"device_id": s.DeviceID, "seq": seq, "head": head})
	status, out, err := postJSON(serverBase(apiURL)+"/api/v1/struggle/anchors", apiKey, body, nil)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("anchor: server returned %d: %s", status, strings.TrimSpace(string(out)))
	}
	return s.markAnchored(seq, now)
}

// Upload sends a bundle's exact bytes. Retrying the same bytes is safe: the
// server deduplicates by digest and returns the existing submission.
func Upload(apiURL, apiKey string, data []byte, digest string) (int, []byte, error) {
	return postJSON(serverBase(apiURL)+"/api/v1/struggle/submissions", apiKey, data, map[string]string{"X-Bundle-Digest": digest})
}

// cleanEvidenceSpool deletes the OpenCode plugin's plaintext evidence files
// once they are fully read into the encrypted store and two days old.
func (s *Store) cleanEvidenceSpool(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil || e.IsDir() || now.Sub(info.ModTime()) < 48*time.Hour {
			continue
		}
		var offset int64
		if s.db.QueryRow(`SELECT offset FROM offsets WHERE path = ?`, path).Scan(&offset) == nil && offset == info.Size() {
			_ = os.Remove(path)
		}
	}
}

type evidenceFlags struct {
	project, from, to, context, terms, exclude, supersedes, store string
	retention                                                     time.Duration
}

func parseEvidenceFlags(name string, args []string, home string) (*flag.FlagSet, *evidenceFlags) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	f := &evidenceFlags{}
	fs.StringVar(&f.project, "project", "", "project name (required for preview and submit)")
	fs.StringVar(&f.from, "from", "", "start date YYYY-MM-DD, local time (default: 7 days ago)")
	fs.StringVar(&f.to, "to", "", "end date YYYY-MM-DD inclusive, local time (default: today)")
	fs.StringVar(&f.context, "context", "", "context you add for the reviewer")
	fs.StringVar(&f.terms, "redact", "", "comma-separated extra words to redact, such as your name or school")
	fs.StringVar(&f.exclude, "exclude", "", "comma-separated evidence item IDs to leave out")
	fs.StringVar(&f.supersedes, "supersedes", "", "digest of a submission returned for relabelling")
	fs.StringVar(&f.store, "store", filepath.Join(home, ".strgl", "evidence.db"), "local evidence database")
	fs.DurationVar(&f.retention, "retention", 30*24*time.Hour, "keep local evidence content this long")
	_ = fs.Parse(args)
	return fs, f
}

func (f *evidenceFlags) options(now time.Time) (BuildOptions, error) {
	day := func(v string, fallback time.Time) (time.Time, error) {
		if v == "" {
			return time.Date(fallback.Year(), fallback.Month(), fallback.Day(), 0, 0, 0, 0, time.Local), nil
		}
		return time.ParseInLocation("2006-01-02", v, time.Local)
	}
	from, err := day(f.from, now.AddDate(0, 0, -7))
	if err != nil {
		return BuildOptions{}, err
	}
	to, err := day(f.to, now)
	if err != nil {
		return BuildOptions{}, err
	}
	opt := BuildOptions{Project: f.project, From: from, To: to.AddDate(0, 0, 1), UserContext: f.context, Supersedes: f.supersedes, Now: now, Exclude: map[string]bool{}}
	for _, id := range strings.Split(f.exclude, ",") {
		if id = strings.TrimSpace(id); id != "" {
			opt.Exclude[id] = true
		}
	}
	if f.terms != "" {
		opt.Terms = strings.Split(f.terms, ",")
	}
	return opt, nil
}

// runEvidenceCommand handles the timeline, bundle and preview
// subcommands. It returns false when args name no such command.
func runEvidenceCommand(args []string, home, cfgPath string) bool {
	if len(args) == 0 {
		return false
	}
	cmd := args[0]
	switch cmd {
	case "timeline", "bundle", "preview":
	default:
		return false
	}
	_, f := parseEvidenceFlags(cmd, args[1:], home)
	store, err := OpenStore(f.store, f.retention)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	// Read whatever the harnesses wrote since the last scan first.
	if _, err := store.CaptureScan(defaultCaptureSources(home), now.Add(-7*24*time.Hour), now); err != nil {
		log.Printf("capture: %v", err)
	}
	opt, err := f.options(now)
	if err != nil {
		log.Fatal(err)
	}
	switch cmd {
	case "timeline":
		turns, err := store.Turns(opt.Project, opt.From, opt.To)
		if err != nil {
			log.Fatal(err)
		}
		labels, items, err := store.labelTurns(turns)
		if err != nil {
			log.Fatal(err)
		}
		type row struct {
			Turn
			Label Label    `json:"label"`
			Items []string `json:"items"` // kind and size only: the timeline never prints raw text
		}
		var out []row
		for i, t := range turns {
			r := row{Turn: t, Label: labels[i]}
			for _, it := range items[t.ID] {
				r.Items = append(r.Items, fmt.Sprintf("%s %s (%d chars)", it.ID, it.Item.Kind, len(it.Item.Text)))
			}
			out = append(out, r)
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(data))
	case "bundle":
		requireProject(opt)
		b, err := store.BuildBundle(opt)
		if err != nil {
			log.Fatal(err)
		}
		data, digest := b.Encode()
		os.Stdout.Write(data)
		fmt.Fprintf(os.Stderr, "sha256 %s\n", digest)
	case "preview":
		requireProject(opt)
		apiURL, apiKey := readConfig(cfgPath)
		if err := servePreview(store, opt, apiURL, apiKey); err != nil {
			log.Fatal(err)
		}
	}
	return true
}

func requireProject(opt BuildOptions) {
	if opt.Project == "" {
		log.Fatal("-project is required")
	}
}
