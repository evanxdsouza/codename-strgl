// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the local turn timeline and encrypted evidence database
// (~/.strgl/evidence.db). Turn metadata is plain; evidence content is
// AES-256-GCM encrypted with a key kept outside the database. Each item also
// gets a salted commitment chained to the previous one, so later alteration
// of local evidence is detectable once a chain head has been anchored.
type Store struct {
	db             *sql.DB
	aead           cipher.AEAD
	salt           []byte
	DeviceID       string
	KeyProtection  string
	retention      time.Duration
	maxContentSize int64
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS offsets(path TEXT PRIMARY KEY, offset INTEGER NOT NULL, context TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, harness TEXT NOT NULL, open_turn TEXT, last_turn TEXT);
CREATE TABLE IF NOT EXISTS turns(
  id TEXT PRIMARY KEY, harness TEXT NOT NULL, session TEXT NOT NULL, project TEXT NOT NULL, branch TEXT NOT NULL,
  cwd TEXT NOT NULL, root TEXT NOT NULL, model TEXT NOT NULL, version TEXT NOT NULL,
  started_at INTEGER NOT NULL, last_activity_at INTEGER NOT NULL, ended_at INTEGER, end_reason TEXT NOT NULL DEFAULT 'open',
  prompt_length INTEGER NOT NULL, agent_events INTEGER NOT NULL DEFAULT 0, tool_calls INTEGER NOT NULL DEFAULT 0,
  child_sessions INTEGER NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
  recorded_at INTEGER NOT NULL, backfilled INTEGER NOT NULL, changed_files TEXT NOT NULL DEFAULT '[]', diff_observed INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS turns_by_project ON turns(project, started_at);
CREATE TABLE IF NOT EXISTS items(
  id TEXT PRIMARY KEY, turn_id TEXT NOT NULL, kind TEXT NOT NULL, occurred_at INTEGER NOT NULL, recorded_at INTEGER NOT NULL,
  size INTEGER NOT NULL, nonce BLOB, ciphertext BLOB, commitment TEXT NOT NULL, chain_seq INTEGER NOT NULL, purged_at INTEGER);
CREATE INDEX IF NOT EXISTS items_by_turn ON items(turn_id, occurred_at);
CREATE TABLE IF NOT EXISTS chain(
  seq INTEGER PRIMARY KEY, item_id TEXT NOT NULL, commitment TEXT NOT NULL, head TEXT NOT NULL,
  recorded_at INTEGER NOT NULL, anchored_at INTEGER);
CREATE TABLE IF NOT EXISTS snapshots(root TEXT PRIMARY KEY, files TEXT NOT NULL, head TEXT NOT NULL, taken_at INTEGER NOT NULL, nonce BLOB, contents BLOB);
`

// OpenStore opens or creates the store. The key comes from the operating
// system's protection where available (see keystore_*.go).
func OpenStore(path string, retention time.Duration) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	key, protection, err := loadOrCreateKey(filepath.Dir(path))
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("evidence key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		db.Close()
		return nil, err
	}
	aead, _ := cipher.NewGCM(block)
	s := &Store{db: db, aead: aead, KeyProtection: protection, retention: retention, maxContentSize: 256 << 20}
	saltHex, err := s.metaOrCreate("salt", func() string { return randomHex(32) })
	if err != nil {
		db.Close()
		return nil, err
	}
	s.salt, _ = hex.DecodeString(saltHex)
	if s.DeviceID, err = s.metaOrCreate("device_id", func() string { return "dev_" + randomHex(12) }); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) metaOrCreate(key string, create func() string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		value = create()
		_, err = s.db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, key, value)
	}
	return value, err
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// mac is the per-install salted hash used for commitments and redaction
// placeholders. Without the salt, short values cannot be guessed from it.
func (s *Store) mac(parts ...string) string {
	h := hmac.New(sha256.New, s.salt)
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) encrypt(id string, plain []byte) (nonce, sealed []byte) {
	nonce = make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return nonce, s.aead.Seal(nil, nonce, plain, []byte(id))
}

func (s *Store) decrypt(id string, nonce, sealed []byte) ([]byte, error) {
	return s.aead.Open(nil, nonce, sealed, []byte(id))
}

// chainHead links a commitment to the previous head.
func chainHead(prev, commitment string, recordedAt int64) string {
	sum := sha256.Sum256([]byte(prev + "\n" + commitment + "\n" + strconv.FormatInt(recordedAt, 10)))
	return hex.EncodeToString(sum[:])
}

// addItem stores one evidence item once; replays of the same line are
// ignored because item IDs derive from the line's content.
func (s *Store) addItem(tx *sql.Tx, id, turnID string, item Item, occurred, recorded int64) error {
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM items WHERE id = ?`, id).Scan(&exists); err != nil || exists > 0 {
		return err
	}
	plain, _ := json.Marshal(item)
	commitment := s.mac("item", id, item.Kind, string(plain))
	var seq int64
	var prev string
	err := tx.QueryRow(`SELECT seq, head FROM chain ORDER BY seq DESC LIMIT 1`).Scan(&seq, &prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	seq++
	head := chainHead(prev, commitment, recorded)
	nonce, sealed := s.encrypt(id, plain)
	if _, err := tx.Exec(`INSERT INTO chain(seq, item_id, commitment, head, recorded_at) VALUES (?, ?, ?, ?, ?)`, seq, id, commitment, head, recorded); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO items(id, turn_id, kind, occurred_at, recorded_at, size, nonce, ciphertext, commitment, chain_seq) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, turnID, item.Kind, occurred, recorded, len(plain), nonce, sealed, commitment, seq)
	return err
}

// StoredItem is an evidence item read back from the store.
type StoredItem struct {
	ID         string
	TurnID     string
	OccurredAt int64
	RecordedAt int64
	Commitment string
	ChainSeq   int64
	Purged     bool
	Item       Item
}

func (s *Store) itemsForTurn(turnID string) ([]StoredItem, error) {
	rows, err := s.db.Query(`SELECT id, kind, occurred_at, recorded_at, commitment, chain_seq, nonce, ciphertext, purged_at FROM items WHERE turn_id = ? ORDER BY occurred_at, chain_seq`, turnID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredItem
	for rows.Next() {
		var it StoredItem
		var kind string
		var nonce, sealed []byte
		var purged sql.NullInt64
		if err := rows.Scan(&it.ID, &kind, &it.OccurredAt, &it.RecordedAt, &it.Commitment, &it.ChainSeq, &nonce, &sealed, &purged); err != nil {
			return nil, err
		}
		it.TurnID, it.Purged = turnID, purged.Valid
		it.Item.Kind = kind
		if !it.Purged {
			plain, err := s.decrypt(it.ID, nonce, sealed)
			if err != nil {
				return nil, fmt.Errorf("decrypt %s: %w", it.ID, err)
			}
			if err := json.Unmarshal(plain, &it.Item); err != nil {
				return nil, err
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Turn is one human prompt and the agent activity that followed it.
type Turn struct {
	ID             string `json:"id"`
	Harness        string `json:"harness"`
	Session        string `json:"session"`
	Project        string `json:"project"`
	Branch         string `json:"branch"`
	Model          string `json:"model"`
	Version        string `json:"version"`
	StartedAt      int64  `json:"started_at"` // milliseconds since the Unix epoch
	LastActivityAt int64  `json:"last_activity_at"`
	EndedAt        *int64 `json:"ended_at"`
	EndReason      string `json:"end_reason"` // open, completed, interrupted, superseded
	PromptLength   int    `json:"prompt_length"`
	AgentEvents    int    `json:"agent_events"`
	ToolCalls      int    `json:"tool_calls"`
	ChildSessions  int    `json:"child_sessions"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	RecordedAt     int64  `json:"recorded_at"`
	Backfilled     bool   `json:"backfilled"`
	// DiffObserved is true when the collector snapshotted the repository
	// while this turn was active, so an empty ChangedFiles means no change.
	DiffObserved bool     `json:"diff_observed"`
	ChangedFiles []string `json:"changed_files"`
	root         string
}

const turnColumns = `id, harness, session, project, branch, model, version, started_at, last_activity_at, ended_at, end_reason, prompt_length,
  agent_events, tool_calls, child_sessions, input_tokens, output_tokens, recorded_at, backfilled, root, diff_observed, changed_files`

func scanTurn(row interface{ Scan(...any) error }) (Turn, error) {
	var t Turn
	var ended sql.NullInt64
	var changed string
	err := row.Scan(&t.ID, &t.Harness, &t.Session, &t.Project, &t.Branch, &t.Model, &t.Version, &t.StartedAt, &t.LastActivityAt, &ended,
		&t.EndReason, &t.PromptLength, &t.AgentEvents, &t.ToolCalls, &t.ChildSessions, &t.InputTokens, &t.OutputTokens, &t.RecordedAt,
		&t.Backfilled, &t.root, &t.DiffObserved, &changed)
	_ = json.Unmarshal([]byte(changed), &t.ChangedFiles)
	if ended.Valid {
		t.EndedAt = &ended.Int64
	}
	return t, err
}

// Turns lists turns of a project (all projects when empty) that started in
// [from, to), oldest first.
func (s *Store) Turns(project string, from, to time.Time) ([]Turn, error) {
	rows, err := s.db.Query(`SELECT `+turnColumns+` FROM turns WHERE (? = '' OR project = ?) AND started_at >= ? AND started_at < ? ORDER BY started_at, id`,
		project, project, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Turn
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// earlierPrompts returns the prompt items of a session's turns before t,
// oldest first, for novelty and repetition features.
func (s *Store) earlierPrompts(t Turn) ([]StoredItem, error) {
	rows, err := s.db.Query(`SELECT id FROM turns WHERE session = ? AND harness = ? AND (started_at < ? OR (started_at = ? AND id < ?)) ORDER BY started_at, id`,
		t.Session, t.Harness, t.StartedAt, t.StartedAt, t.ID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []StoredItem
	for _, id := range ids {
		items, err := s.itemsForTurn(id)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Item.Kind == "prompt" {
				out = append(out, it)
			}
		}
	}
	return out, nil
}

func (s *Store) previousTurn(t Turn) (*Turn, error) {
	row := s.db.QueryRow(`SELECT `+turnColumns+` FROM turns WHERE session = ? AND harness = ? AND (started_at < ? OR (started_at = ? AND id < ?)) ORDER BY started_at DESC, id DESC LIMIT 1`,
		t.Session, t.Harness, t.StartedAt, t.StartedAt, t.ID)
	prev, err := scanTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &prev, err
}

// Projects lists the projects with recorded turns.
func (s *Store) Projects() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT project FROM turns ORDER BY project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ChainEntry is one link of the commitment chain, without content.
type ChainEntry struct {
	Seq        int64  `json:"seq"`
	Commitment string `json:"commitment"`
	RecordedAt int64  `json:"recorded_at"`
}

// chainSegment returns the entries from..to and the head before from, so a
// server can recompute every head in between and compare its anchors.
func (s *Store) chainSegment(from, to int64) (string, []ChainEntry, error) {
	var prev string
	if from > 1 {
		if err := s.db.QueryRow(`SELECT head FROM chain WHERE seq = ?`, from-1).Scan(&prev); err != nil {
			return "", nil, err
		}
	}
	rows, err := s.db.Query(`SELECT seq, commitment, recorded_at FROM chain WHERE seq BETWEEN ? AND ? ORDER BY seq`, from, to)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var out []ChainEntry
	for rows.Next() {
		var e ChainEntry
		if err := rows.Scan(&e.Seq, &e.Commitment, &e.RecordedAt); err != nil {
			return "", nil, err
		}
		out = append(out, e)
	}
	return prev, out, rows.Err()
}

// unanchoredHead returns the newest chain head not yet anchored.
func (s *Store) unanchoredHead() (seq int64, head string, ok bool) {
	err := s.db.QueryRow(`SELECT seq, head FROM chain WHERE anchored_at IS NULL ORDER BY seq DESC LIMIT 1`).Scan(&seq, &head)
	return seq, head, err == nil
}

func (s *Store) markAnchored(seq int64, at time.Time) error {
	_, err := s.db.Exec(`UPDATE chain SET anchored_at = ? WHERE seq <= ? AND anchored_at IS NULL`, at.UnixMilli(), seq)
	return err
}

// Purge drops evidence content older than the retention period, then the
// oldest content beyond the size cap. Commitments stay, so the chain still
// verifies; purged items appear as "purged" in a submission.
func (s *Store) Purge(now time.Time) (int64, error) {
	res, err := s.db.Exec(`UPDATE items SET nonce = NULL, ciphertext = NULL, purged_at = ? WHERE purged_at IS NULL AND occurred_at < ?`,
		now.UnixMilli(), now.Add(-s.retention).UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	var total int64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM items WHERE purged_at IS NULL`).Scan(&total)
	for total > s.maxContentSize {
		var id string
		var size int64
		if err := s.db.QueryRow(`SELECT id, size FROM items WHERE purged_at IS NULL ORDER BY occurred_at LIMIT 1`).Scan(&id, &size); err != nil {
			break
		}
		if _, err := s.db.Exec(`UPDATE items SET nonce = NULL, ciphertext = NULL, purged_at = ? WHERE id = ?`, now.UnixMilli(), id); err != nil {
			return n, err
		}
		total -= size
		n++
	}
	return n, nil
}
