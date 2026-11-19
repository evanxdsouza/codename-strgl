// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// Project is where an event's working directory belongs: the enclosing git
// repository, or the directory itself when it is not in one.
type Project struct {
	Name   string
	Branch string
}

var projectCache = map[string]Project{}

func projectFor(cwd string) Project {
	if cwd == "" {
		return Project{}
	}
	if p, ok := projectCache[cwd]; ok {
		return p
	}
	p := Project{Name: filepath.Base(filepath.Clean(cwd))}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		if gitDir, ok := findGitDir(dir); ok {
			p = Project{Name: filepath.Base(dir), Branch: readBranch(gitDir)}
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	projectCache[cwd] = p
	return p
}

// findGitDir returns dir/.git, following the "gitdir:" file that worktrees
// and submodules use.
func findGitDir(dir string) (string, bool) {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return gitPath, true
	}
	data, err := os.ReadFile(gitPath)
	if err != nil || !strings.HasPrefix(string(data), "gitdir:") {
		return "", false
	}
	target := strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return target, true
}

func readBranch(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(data)), "ref: refs/heads/")
}
