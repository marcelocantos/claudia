// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const (
	// dataKeyLen is an AES-256 key.
	dataKeyLen = 32
	// dataFileVersion leads the file so a later format can be told apart.
	dataFileVersion byte = 1
	dataFileName         = "plan-credentials.enc"
)

// testDataPath is the plan file a test binary uses (🎯T143).
var testDataPath string

// UseTestDataPath points this test binary's plan file at path. Test mains
// call it; a test binary that does not has no plan file at all.
func UseTestDataPath(path string) { testDataPath = path }

// DefaultDataPath is the encrypted plan file for this user. It does not
// follow XDG_STATE_HOME: the Keychain key is per user, so the file it
// opens must not move with a process environment.
//
// A test binary never gets the real file (🎯T143): on 2026-09-29 a test's
// Flush resealed the owner's plan file with a key only its fake Keychain
// held, and every plan login was lost.
func DefaultDataPath() (string, error) {
	if testing.Testing() {
		if testDataPath == "" {
			return "", errors.New("omp: a test binary never uses the real plan data file; call omp.UseTestDataPath (🎯T143)")
		}
		return testDataPath, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("omp: locate plan data dir: %w", err)
	}
	return filepath.Join(dir, "claudia", dataFileName), nil
}

// readDataFile decrypts the plan file. A missing file is an empty Item,
// as a missing Keychain item is.
func readDataFile(path string, key []byte) (Item, error) {
	if path == "" {
		return Item{}, fmt.Errorf("omp: plan data path is required")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Item{Records: map[string]Record{}}, nil
	}
	if err != nil {
		return Item{}, fmt.Errorf("omp: read plan data: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return Item{}, err
	}
	if len(raw) < 1+gcm.NonceSize() || raw[0] != dataFileVersion {
		return Item{}, fmt.Errorf("omp: plan data file %s is not a version %d file", path, dataFileVersion)
	}
	nonce, sealed := raw[1:1+gcm.NonceSize()], raw[1+gcm.NonceSize():]
	blob, err := gcm.Open(nil, nonce, sealed, []byte(KeychainService))
	if err != nil {
		return Item{}, fmt.Errorf("omp: plan data file %s does not match the Keychain key", path)
	}
	var item Item
	if err := json.Unmarshal(blob, &item); err != nil {
		return Item{}, fmt.Errorf("omp: plan data is not the plan blob: %w", err)
	}
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	return item, nil
}

// writeDataFile encrypts blob and replaces the file atomically.
func writeDataFile(path string, key, blob []byte) error {
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	out := make([]byte, 1+gcm.NonceSize(), 1+gcm.NonceSize()+len(blob)+gcm.Overhead())
	out[0] = dataFileVersion
	if _, err := rand.Read(out[1:]); err != nil {
		return fmt.Errorf("omp: plan data nonce: %w", err)
	}
	out = gcm.Seal(out, out[1:], blob, []byte(KeychainService))

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("omp: plan data dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, dataFileName+".*")
	if err != nil {
		return fmt.Errorf("omp: plan data temp: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("omp: write plan data: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("omp: sync plan data: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("omp: close plan data: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("omp: replace plan data: %w", err)
	}
	return nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("omp: plan data key: %w", err)
	}
	return cipher.NewGCM(block)
}
