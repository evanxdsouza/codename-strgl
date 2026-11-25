// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Snapshot is the working tree state of a git repository at one moment:
// a hash per changed or untracked file, relative to HEAD.
type Snapshot struct {
	Head    string            `json:"head"`
	Files   map[string]string `json:"files"`
	TakenAt int64             `json:"taken_at"`
	// contents holds the current text of changed files, so the next scan
	// can show exactly what changed since this one. Stored encrypted.
	contents map[string]string
}

const (
	maxUntracked     = 200
	maxUntrackedSize = 1 << 20
)

func git(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "core.quotepath=off"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	return cmd.Output()
}

// takeSnapshot reads the repository without changing it (no index refresh).
func takeSnapshot(root string, now time.Time) (*Snapshot, bool) {
	head, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		return nil, false
	}
	snap := &Snapshot{Head: strings.TrimSpace(string(head)), Files: map[string]string{}, TakenAt: now.UnixMilli(), contents: map[string]string{}}
	// Files that differ from HEAD, hashed by their content on disk.
	if out, err := git(root, "diff", "HEAD", "--name-only", "-z", "--no-renames"); err == nil {
		for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
			if name == "" {
				continue
			}
			data, err := os.ReadFile(filepath.Join(root, name))
			switch {
			case err != nil:
				snap.Files[name], snap.contents[name] = "deleted", ""
			default:
				sum := sha256.Sum256(data)
				snap.Files[name] = hex.EncodeToString(sum[:8])
				if len(data) <= maxUntrackedSize && bytes.IndexByte(data, 0) < 0 {
					snap.contents[name] = string(data)
				}
			}
		}
	}
	if out, err := git(root, "ls-files", "--others", "--exclude-standard", "-z"); err == nil {
		names := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
		for i, name := range names {
			if name == "" || i >= maxUntracked {
				break
			}
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || len(data) > maxUntrackedSize {
				continue
			}
			sum := sha256.Sum256(data)
			snap.Files[name] = "new:" + hex.EncodeToString(sum[:8])
			if bytes.IndexByte(data, 0) < 0 {
				snap.contents[name] = string(data)
			}
		}
	}
	return snap, true
}

// changedFiles lists files whose state differs between two snapshots.
// A commit in between shows its files as changed too.
func changedFiles(before, after *Snapshot) []string {
	var out []string
	for name, hash := range after.Files {
		if before.Files[name] != hash {
			out = append(out, name)
		}
	}
	for name := range before.Files {
		if _, ok := after.Files[name]; !ok && before.Head == after.Head {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// contentAt is a file's text in a snapshot: its captured content when it
// differed from HEAD then, otherwise its content at that HEAD.
func contentAt(root string, snap *Snapshot, name string) (string, bool) {
	if _, dirty := snap.Files[name]; dirty {
		c, ok := snap.contents[name]
		return c, ok
	}
	out, err := git(root, "show", snap.Head+":"+name)
	if err != nil {
		return "", true // did not exist
	}
	if bytes.IndexByte(out, 0) >= 0 || len(out) > maxUntrackedSize {
		return "", false
	}
	return string(out), true
}

// changedFilesPrefix starts every change item; DraftDevlog reads the file
// names after it.
const changedFilesPrefix = "Files changed since the previous scan: "

// diffEvidence shows what changed in each file between two scans, not
// against HEAD, so earlier changes by someone else are not repeated.
func diffEvidence(root string, before, after *Snapshot, files []string) string {
	var b strings.Builder
	b.WriteString(changedFilesPrefix + strings.Join(files, ", ") + "\n")
	if before.Head != after.Head {
		b.WriteString("HEAD moved to " + after.Head + " since the previous scan (a commit was made or checked out).\n")
	}
	for _, name := range files {
		old, ok1 := contentAt(root, before, name)
		cur, ok2 := contentAt(root, after, name)
		var d string
		if !ok1 || !ok2 {
			d = "--- " + name + " (binary or too large; content not captured)\n"
		} else {
			d = "--- " + name + "\n" + lineDiff(old, cur)
		}
		if b.Len()+len(d) > 20000 {
			b.WriteString("[further diffs omitted: size cap]\n")
			break
		}
		b.WriteString(d)
	}
	return b.String()
}

// lineDiff lists removed (-) and added (+) lines from a longest common
// subsequence of lines. Unchanged lines are left out.
func lineDiff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	if a == "" {
		x = nil
	}
	if b == "" {
		y = nil
	}
	if len(x)*len(y) > 4_000_000 {
		return "(file too large to diff line by line)\n"
	}
	lcs := make([][]int32, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			out.WriteString("+" + y[j] + "\n")
			j++
		default:
			out.WriteString("-" + x[i] + "\n")
			i++
		}
	}
	return out.String()
}
