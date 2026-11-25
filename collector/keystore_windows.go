// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package main

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: the key file is encrypted with DPAPI for the current user, so
// another account on the machine cannot decrypt it.
func loadOrCreateKey(dir string) ([]byte, string, error) {
	path := filepath.Join(dir, "evidence.key.dpapi")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, "", err
		}
		sealed, err := dpapi(key, true)
		if err != nil {
			return nil, "", err
		}
		return key, "windows-dpapi", os.WriteFile(path, sealed, 0o600)
	}
	if err != nil {
		return nil, "", err
	}
	key, err := dpapi(data, false)
	return key, "windows-dpapi", err
}

func dpapi(in []byte, protect bool) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}
	var output windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output)
	} else {
		err = windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}
