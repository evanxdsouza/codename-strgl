// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// fileKey keeps the evidence key in a file readable only by the owner. It is
// the fallback where no OS key protection is available.
func fileKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, "evidence.key")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := randomHex(32)
		if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
			return nil, err
		}
		return hex.DecodeString(key)
	}
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimSpace(string(data)))
}
