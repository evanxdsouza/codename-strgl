// SPDX-License-Identifier: AGPL-3.0-only

//go:build darwin

package main

import (
	"encoding/hex"
	"os/exec"
	"strings"
)

// macOS: the key lives in the login Keychain, read with the security tool.
// Falls back to an owner-only key file when the Keychain is unavailable.
func loadOrCreateKey(dir string) ([]byte, string, error) {
	const service, account = "strgl-evidence", "strgl-collector"
	if out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output(); err == nil {
		if key, err := hex.DecodeString(strings.TrimSpace(string(out))); err == nil && len(key) == 32 {
			return key, "macos-keychain", nil
		}
	}
	key := randomHex(32)
	if exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", account, "-w", key).Run() == nil {
		k, _ := hex.DecodeString(key)
		return k, "macos-keychain", nil
	}
	k, err := fileKey(dir)
	return k, "file-0600", err
}
