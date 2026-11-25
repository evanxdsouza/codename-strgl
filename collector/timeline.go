// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// backfillLag is how long after the event a record may be written before it
// counts as backfilled: read from an older transcript, not observed live.
const backfillLag = 10 * time.Minute

// standaloneHarness names turns made of the person's own file changes with
// no agent prompt: editing in an editor, a terminal or another tool. They are
// observed only as changes between repository snapshots.
const standaloneHarness = "editor"

// standaloneAfter is how long a repository must be quiet before the changes
// the person made in it, with no prompt, are closed into standalone turns.
// Changes newer than this still belong to the prompt that may follow them.
const standaloneAfter = 10 * time.Minute

// standaloneWatch is how long after its last activity a repository keeps
// being snapshotted, and maxWatchedRepos caps how many are.
const (
	standaloneWatch = 24 * time.Hour
	maxWatchedRepos = 8
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// CaptureScan reads every transcript line written since the last capture
// scan into the turn timeline. It is separate from the heartbeat scan, so
// sending heartbeats and recording evidence cannot disturb each other. Lines
// of all files of a source are applied in time order, so a child session's
// file is never applied before its parent's prompt.
func (s *Store) CaptureScan(sources []captureSource, since, now time.Time) (int, error) {
	added := 0
	for _, src := range sources {
		var pending []pendingCapture
		var files []fileProgress
		_ = filepath.WalkDir(src.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			fp, caps, err := s.readCaptures(src, path, since)
			if err == nil && fp != nil {
				files = append(files, *fp)
				pending = append(pending, caps...)
			}
			return nil
		})
		if len(files) == 0 {
			continue
		}
		sort.SliceStable(pending, func(i, j int) bool { return pending[i].c.Time.Before(pending[j].c.Time) })
		tx, err := s.db.Begin()
		if err != nil {
			return added, err
		}
		for _, p := range pending {
			if err := s.apply(tx, src.harness, p.lineID, p.c, now); err != nil {
				tx.Rollback()
				return added, err
			}
			added++
		}
		for _, f := range files {
			ctxData, _ := json.Marshal(f.ctx)
			if _, err := tx.Exec(`INSERT INTO offsets(path, offset, context) VALUES (?, ?, ?) ON CONFLICT(path) DO UPDATE SET offset = excluded.offset, context = excluded.context`,
				f.path, f.offset, string(ctxData)); err != nil {
				tx.Rollback()
				return added, err
			}
		}
		if err := tx.Commit(); err != nil {
			return added, err
		}
	}
	if err := s.closeIdle(now); err != nil {
		return added, err
	}
	if err := s.snapshotRepos(now); err != nil {
		return added, err
	}
	return added, s.closeStandalone(now)
}

type pendingCapture struct {
	lineID string
	c      Capture
}

type fileProgress struct {
	path   string
	offset int64
	ctx    fileContext
}

// idleAfter ends a turn whose agent has been quiet this long. Claude Code
// writes no completion record, so this is how its turns end.
const idleAfter = 2 * time.Minute

func (s *Store) closeIdle(now time.Time) error {
	rows, err := s.db.Query(`SELECT id, harness, session, root, last_activity_at, backfilled FROM turns WHERE ended_at IS NULL AND last_activity_at < ?`, now.Add(-idleAfter).UnixMilli())
	if err != nil {
		return err
	}
	type idle struct {
		id, harness, session, root string
		last                       int64
		backfilled                 bool
	}
	var list []idle
	for rows.Next() {
		var t idle
		if err := rows.Scan(&t.id, &t.harness, &t.session, &t.root, &t.last, &t.backfilled); err != nil {
			rows.Close()
			return err
		}
		list = append(list, t)
	}
	rows.Close()
	for _, t := range list {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if err := s.closeTurn(tx, t.id, t.last, "idle"); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`UPDATE sessions SET open_turn = NULL WHERE open_turn = ?`, t.id); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// readCaptures parses the lines appended to one file since its saved offset.
func (s *Store) readCaptures(src captureSource, path string, since time.Time) (*fileProgress, []pendingCapture, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	fp := &fileProgress{path: path}
	var ctxJSON string
	err = s.db.QueryRow(`SELECT offset, context FROM offsets WHERE path = ?`, path).Scan(&fp.offset, &ctxJSON)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if info.ModTime().Before(since) {
			fp.offset = info.Size()
		}
	case err != nil:
		return nil, nil, err
	default:
		_ = json.Unmarshal([]byte(ctxJSON), &fp.ctx)
		if info.Size() == fp.offset {
			return nil, nil, nil
		}
	}
	if info.Size() < fp.offset {
		fp.offset, fp.ctx = 0, fileContext{} // rewritten
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	if _, err := f.Seek(fp.offset, io.SeekStart); err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReaderSize(f, 1<<20)
	var out []pendingCapture
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break // a partial last line is read again next scan
		}
		lineOffset := fp.offset
		fp.offset += int64(len(line))
		c, ok := src.parse(bytes.TrimSpace(line), &fp.ctx)
		if !ok || c.Time.Before(since) || c.Session == "" {
			continue
		}
		out = append(out, pendingCapture{s.mac("line", path, strconv.FormatInt(lineOffset, 10))[:24], c})
	}
	return fp, out, nil
}

// apply adds one capture to the timeline. A genuine human prompt opens a
// turn; a child agent session's activity belongs to its parent's turn; other
// agent activity belongs to the session's open (or else latest) turn.
func (s *Store) apply(tx *sql.Tx, harness, lineID string, c Capture, now time.Time) error {
	at := c.Time.UnixMilli()
	session := c.Session
	child := false
	if c.Parent != "" {
		session, child = c.Parent, true
	}
	var openTurn, lastTurn sql.NullString
	err := tx.QueryRow(`SELECT open_turn, last_turn FROM sessions WHERE id = ?`, harness+"|"+session).Scan(&openTurn, &lastTurn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	backfilled := now.Sub(c.Time) > backfillLag

	if c.Human && !child {
		turnID := "turn_" + s.mac("turn", harness, c.Session, strconv.FormatInt(at, 10), lineID)[:20]
		var exists int
		_ = tx.QueryRow(`SELECT COUNT(*) FROM turns WHERE id = ?`, turnID).Scan(&exists)
		if exists > 0 {
			return nil
		}
		project := projectFor(c.Cwd)
		root := gitRoot(c.Cwd)
		if openTurn.Valid {
			if err := s.closeTurn(tx, openTurn.String, at, "superseded"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO turns(id, harness, session, project, branch, cwd, root, model, version, started_at, last_activity_at,
			prompt_length, recorded_at, backfilled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			turnID, harness, c.Session, project.Name, project.Branch, c.Cwd, root, c.Model, c.Version, at, at,
			c.PromptLength, now.UnixMilli(), backfilled); err != nil {
			return err
		}
		// Changes the person made while no agent was working lead up to
		// this prompt.
		if root != "" {
			if _, err := tx.Exec(`UPDATE items SET turn_id = ? WHERE turn_id = ? AND occurred_at <= ? AND occurred_at >= ?`, turnID, "pending:"+root,
				at+int64(time.Minute/time.Millisecond), at-standaloneAfter.Milliseconds()); err != nil {
				return err
			}
		}
		for i, item := range c.Items {
			if err := s.addItem(tx, lineID+":"+strconv.Itoa(i), turnID, item, at, now.UnixMilli()); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO sessions(id, harness, open_turn, last_turn) VALUES (?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET open_turn = excluded.open_turn, last_turn = excluded.last_turn`, harness+"|"+session, harness, turnID, turnID)
		return err
	}

	// Agent activity. Without any earlier turn in this session it still gets
	// a turn, marked with no prompt, so autonomous work is visible rather
	// than silently dropped or credited.
	turnID := openTurn.String
	if !openTurn.Valid && lastTurn.Valid && !child {
		turnID = lastTurn.String
		// A long quiet tool run looks idle; resume the turn when the agent
		// continues. A completed or interrupted turn stays ended.
		res, err := tx.Exec(`UPDATE turns SET ended_at = NULL, end_reason = 'open' WHERE id = ? AND end_reason = 'idle'`, turnID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			if _, err := tx.Exec(`UPDATE sessions SET open_turn = ? WHERE id = ?`, turnID, harness+"|"+session); err != nil {
				return err
			}
			openTurn = sql.NullString{String: turnID, Valid: true}
		}
	} else if !openTurn.Valid {
		turnID = lastTurn.String
	}
	if turnID == "" {
		turnID = "turn_" + s.mac("agent-turn", harness, session)[:20]
		project := projectFor(c.Cwd)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO turns(id, harness, session, project, branch, cwd, root, model, version, started_at, last_activity_at,
			prompt_length, recorded_at, backfilled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			turnID, harness, session, project.Name, project.Branch, c.Cwd, gitRoot(c.Cwd), c.Model, c.Version, at, at, now.UnixMilli(), backfilled); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO sessions(id, harness, open_turn, last_turn) VALUES (?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET open_turn = excluded.open_turn, last_turn = excluded.last_turn`, harness+"|"+session, harness, turnID, turnID); err != nil {
			return err
		}
		openTurn = sql.NullString{String: turnID, Valid: true}
	}
	tools := 0
	for i, item := range c.Items {
		if item.Kind == "tool_call" {
			tools++
		}
		if err := s.addItem(tx, lineID+":"+strconv.Itoa(i), turnID, item, at, now.UnixMilli()); err != nil {
			return err
		}
	}
	childInc := 0
	if child {
		var seen int
		_ = tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, harness+"|child|"+c.Session).Scan(&seen)
		if seen == 0 {
			childInc = 1
			if _, err := tx.Exec(`INSERT INTO sessions(id, harness, last_turn) VALUES (?, ?, ?)`, harness+"|child|"+c.Session, harness, turnID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE turns SET last_activity_at = MAX(last_activity_at, ?), agent_events = agent_events + 1, tool_calls = tool_calls + ?,
		child_sessions = child_sessions + ?, input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
		model = CASE WHEN ? != '' THEN ? ELSE model END WHERE id = ?`,
		at, tools, childInc, c.InputTokens, c.OutputTokens, c.Model, c.Model, turnID); err != nil {
		return err
	}
	if c.End && !child && openTurn.Valid && openTurn.String == turnID {
		reason := "completed"
		if c.Interrupted {
			reason = "interrupted"
		}
		if err := s.closeTurn(tx, turnID, at, reason); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE sessions SET open_turn = NULL WHERE id = ?`, harness+"|"+session)
		return err
	}
	return nil
}

// closeTurn ends a turn once.
func (s *Store) closeTurn(tx *sql.Tx, turnID string, at int64, reason string) error {
	_, err := tx.Exec(`UPDATE turns SET ended_at = ?, end_reason = ?, last_activity_at = MAX(last_activity_at, ?) WHERE id = ? AND ended_at IS NULL`,
		at, reason, at, turnID)
	return err
}

// snapshotRepos snapshots each repository with recent turns once per scan
// and attributes the files changed since the previous scan. Changes made
// while an agent turn was active belong to that turn as agent changes, even
// if the person also edited then, so a person is never credited for them.
// Changes made while no agent was active are the person's and wait for the
// next prompt in that repository. Accuracy is one scan interval.
func (s *Store) snapshotRepos(now time.Time) error {
	rows, err := s.db.Query(`SELECT root FROM turns WHERE root != '' AND backfilled = 0 AND (ended_at IS NULL OR last_activity_at > ?)
		GROUP BY root ORDER BY MAX(last_activity_at) DESC LIMIT ?`, now.Add(-standaloneWatch).UnixMilli(), maxWatchedRepos)
	if err != nil {
		return err
	}
	var roots []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		roots = append(roots, r)
	}
	rows.Close()
	for _, root := range roots {
		if err := s.snapshotRepo(root, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) snapshotRepo(root string, now time.Time) error {
	snap, ok := takeSnapshot(root, now)
	if !ok {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var files, head string
	var taken int64
	var nonce, sealed []byte
	err = tx.QueryRow(`SELECT files, head, taken_at, nonce, contents FROM snapshots WHERE root = ?`, root).Scan(&files, &head, &taken, &nonce, &sealed)
	if err == nil {
		prev := Snapshot{Head: head, contents: map[string]string{}}
		_ = json.Unmarshal([]byte(files), &prev.Files)
		if plain, err := s.decrypt("snapshot:"+root, nonce, sealed); err == nil {
			_ = json.Unmarshal(plain, &prev.contents)
		}
		active, err := activeTurns(tx, root, taken, now.UnixMilli())
		if err != nil {
			return err
		}
		for _, id := range active {
			if _, err := tx.Exec(`UPDATE turns SET diff_observed = 1 WHERE id = ?`, id); err != nil {
				return err
			}
		}
		if changed := changedFiles(&prev, snap); len(changed) > 0 {
			text := diffEvidence(root, &prev, snap, changed)
			itemID := s.mac("change", root, strconv.FormatInt(taken, 10), strconv.FormatInt(now.UnixMilli(), 10))[:24]
			if len(active) == 0 {
				// A commit or checkout moves HEAD and can bring files that
				// nobody typed, so those changes are not counted as edits.
				kind := "between_turns_change"
				if prev.Head != snap.Head {
					kind = "head_moved_change"
				}
				item := Item{Kind: kind, Text: text}
				if err := s.addItem(tx, itemID, "pending:"+root, item, now.UnixMilli(), now.UnixMilli()); err != nil {
					return err
				}
			} else {
				if len(active) > 1 {
					text = "Several agent turns were active in this repository; these changes may belong to any of them.\n" + text
				}
				if err := s.addItem(tx, itemID, active[0], Item{Kind: "diff", Text: text}, now.UnixMilli(), now.UnixMilli()); err != nil {
					return err
				}
				for _, id := range active {
					if err := mergeChangedFiles(tx, id, changed); err != nil {
						return err
					}
				}
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	s.saveRepoSnapshot(tx, root, snap)
	return tx.Commit()
}

// activeTurns lists turns in root with agent activity in (from, to], newest
// first. A turn's prompt counts as activity.
func activeTurns(tx *sql.Tx, root string, from, to int64) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM turns WHERE root = ? AND harness != ? AND started_at <= ? AND last_activity_at > ? ORDER BY started_at DESC`, root, standaloneHarness, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out, rows.Err()
}

func mergeChangedFiles(tx *sql.Tx, turnID string, files []string) error {
	var current string
	if err := tx.QueryRow(`SELECT changed_files FROM turns WHERE id = ?`, turnID).Scan(&current); err != nil {
		return err
	}
	var list []string
	_ = json.Unmarshal([]byte(current), &list)
	seen := map[string]bool{}
	for _, f := range list {
		seen[f] = true
	}
	for _, f := range files {
		if !seen[f] {
			seen[f] = true
			list = append(list, f)
		}
	}
	sort.Strings(list)
	data, _ := json.Marshal(list)
	_, err := tx.Exec(`UPDATE turns SET changed_files = ? WHERE id = ?`, string(data), turnID)
	return err
}

func (s *Store) saveRepoSnapshot(tx *sql.Tx, root string, snap *Snapshot) {
	files, _ := json.Marshal(snap.Files)
	contents, _ := json.Marshal(snap.contents)
	nonce, sealed := s.encrypt("snapshot:"+root, contents)
	_, _ = tx.Exec(`INSERT INTO snapshots(root, files, head, taken_at, nonce, contents) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(root) DO UPDATE SET files = excluded.files, head = excluded.head, taken_at = excluded.taken_at,
		nonce = excluded.nonce, contents = excluded.contents`, root, string(files), snap.Head, snap.TakenAt, nonce, sealed)
}

func gitRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		if _, ok := findGitDir(dir); ok {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

// closeStandalone turns the person's file changes in a quiet repository into
// standalone turns, so editing with no prompt does not vanish. Each change
// seen by a scan stands for the same two-minute estimate as a heartbeat: a
// turn spans at most two minutes from its first change, and ends at the next
// change anywhere after it (or at its own last change when none follows).
// Anything not caused by typing, such as HEAD moves, is kept as evidence but
// credits no time.
func (s *Store) closeStandalone(now time.Time) error {
	rows, err := s.db.Query(`SELECT id, turn_id, kind, occurred_at FROM items WHERE turn_id LIKE 'pending:%' ORDER BY turn_id, occurred_at, chain_seq`)
	if err != nil {
		return err
	}
	byRoot := map[string][]pendingItem{}
	for rows.Next() {
		var p pendingItem
		var turnID string
		if err := rows.Scan(&p.id, &turnID, &p.kind, &p.at); err != nil {
			rows.Close()
			return err
		}
		root := strings.TrimPrefix(turnID, "pending:")
		byRoot[root] = append(byRoot[root], p)
	}
	rows.Close()
	cutoff := now.Add(-standaloneAfter).UnixMilli()
	for root, list := range byRoot {
		if list[len(list)-1].at > cutoff {
			continue // still quiet for less than standaloneAfter: a prompt may follow
		}
		var detections []int64
		for _, p := range list {
			if p.kind == "between_turns_change" {
				detections = append(detections, p.at)
			}
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if err := s.createStandaloneTurns(tx, root, list, detections, now); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// pendingItem is a change waiting for a prompt that may claim it.
type pendingItem struct {
	id, kind string
	at       int64
}

func (s *Store) createStandaloneTurns(tx *sql.Tx, root string, items []pendingItem, detections []int64, now time.Time) error {
	window := creditWindow.Milliseconds()
	type span struct{ start, end int64 }
	var spans []span
	for i := 0; i < len(detections); {
		start := detections[i]
		j := i
		for j+1 < len(detections) && detections[j+1] < start+window {
			j++
		}
		end := detections[j]
		if j+1 < len(detections) {
			end = start + window
		}
		spans = append(spans, span{start, end})
		i = j + 1
		for i < len(detections) && detections[i] < start+window {
			i++
		}
	}
	project := projectFor(root)
	ids := make([]string, len(spans))
	for i, sp := range spans {
		ids[i] = "turn_" + s.mac("standalone", root, strconv.FormatInt(sp.start, 10))[:20]
		if _, err := tx.Exec(`INSERT OR IGNORE INTO turns(id, harness, session, project, branch, cwd, root, model, version, started_at, last_activity_at,
			ended_at, end_reason, prompt_length, recorded_at, backfilled, diff_observed) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, 'standalone', 0, ?, 0, 1)`,
			ids[i], standaloneHarness, "standalone:"+root, project.Name, project.Branch, root, root, version, sp.start, sp.end, sp.end, now.UnixMilli()); err != nil {
			return err
		}
	}
	for _, it := range items {
		target := "ignored:" + root
		if len(spans) > 0 {
			idx := 0
			for k, sp := range spans {
				if sp.start <= it.at {
					idx = k
				}
			}
			target = ids[idx]
		}
		if _, err := tx.Exec(`UPDATE items SET turn_id = ? WHERE id = ?`, target, it.id); err != nil {
			return err
		}
	}
	for _, id := range ids {
		var files []string
		rows, err := tx.Query(`SELECT id, nonce, ciphertext FROM items WHERE turn_id = ? AND purged_at IS NULL`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var itemID string
			var nonce, sealed []byte
			if err := rows.Scan(&itemID, &nonce, &sealed); err != nil {
				rows.Close()
				return err
			}
			if plain, err := s.decrypt(itemID, nonce, sealed); err == nil {
				var item Item
				if json.Unmarshal(plain, &item) == nil {
					files = append(files, changedFromText(item.Text)...)
				}
			}
		}
		rows.Close()
		sort.Strings(files)
		if err := mergeChangedFiles(tx, id, files); err != nil {
			return err
		}
	}
	return nil
}

// changedFromText reads the file list from the first line of a change item.
func changedFromText(text string) []string {
	const prefix = "Files changed since the previous scan: "
	first, _, _ := strings.Cut(text, "\n")
	if !strings.HasPrefix(first, prefix) {
		return nil
	}
	var out []string
	for _, f := range strings.Split(strings.TrimPrefix(first, prefix), ", ") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
