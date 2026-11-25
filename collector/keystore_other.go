// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows && !darwin

package main

// Linux and other systems: an owner-only (0600) key file. Desktop keyrings
// are not used yet, so anyone who can read your home directory as you can
// decrypt the evidence store.
func loadOrCreateKey(dir string) ([]byte, string, error) {
	key, err := fileKey(dir)
	return key, "file-0600", err
}
