// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 🎯T144: a plan file sealed with a key the Keychain no longer holds (the
// T143 incident) blocks startup, but the owner's recovery sets it aside and
// completes the login under a fresh key.
func TestT144RecoveryReplacesAPlanFileTheKeychainKeyCannotOpen(t *testing.T) {
	ResetKeychainShot()
	t.Cleanup(ResetKeychainShot)
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.enc")
	keychainKey := bytes.Repeat([]byte{1}, dataKeyLen)
	foreignKey := bytes.Repeat([]byte{2}, dataKeyLen)
	if err := writeDataFile(path, foreignKey, []byte(`{"records":{}}`)); err != nil {
		t.Fatal(err)
	}
	item := `{"data_key":"` + hex.EncodeToString(keychainKey) + `"}`
	keyWrites := 0
	store := Store{BrokerPath: "/test/claudia", DataPath: path,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "find-generic-password" {
				return []byte(item), nil
			}
			keyWrites++
			return nil, nil
		},
		RunStdin: func(context.Context, []byte, string, ...string) ([]byte, error) {
			keyWrites++
			return nil, nil
		},
	}
	if err := Open(context.Background(), store); err == nil || !errors.Is(err, errDataFileMismatch) {
		t.Fatalf("startup open = %v; a mismatched file must still be refused", err)
	}

	login := Login{Script: "auth.ts", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[1] == "refresh" {
			return nil, errors.New("omp: no anthropic login in the keychain item")
		}
		return []byte(`{"refresh_token":"r","access_token":"fresh","expiry":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
	}}
	if err := RecoverPlan(context.Background(), store, login, Anthropic); err != nil {
		t.Fatalf("recovery over a mismatched file: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil || got.Records[Anthropic].AccessToken != "fresh" {
		t.Fatalf("recovered login = %+v, %v", got.Records[Anthropic], err)
	}
	// The unreadable file was kept aside, not destroyed.
	entries, _ := os.ReadDir(dir)
	aside := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "plan.enc.unreadable-") {
			aside = true
		}
	}
	if !aside {
		t.Fatalf("the unreadable file was not kept aside: %v", entries)
	}
	// The new file opens with the key recovery wrote to the Keychain.
	if keyWrites == 0 || shot.key == nil {
		t.Fatalf("no fresh key written (writes=%d)", keyWrites)
	}
	if _, err := readDataFile(path, shot.key); err != nil {
		t.Fatalf("the new plan file does not open with the new key: %v", err)
	}
}
