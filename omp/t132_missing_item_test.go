// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestT132MissingItemDiagnosticsAreNotCredentialContents(t *testing.T) {
	missing := errors.New("security: exit status 44")
	denied := errors.New("security: User interaction is not allowed")
	for _, tc := range []struct {
		name    string
		output  string
		runErr  error
		wantErr string
	}{
		{"missing with stderr", "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.", missing, ""},
		{"malformed successful output", "not-json", nil, "not the plan blob"},
		{"read denied", "security: User interaction is not allowed", denied, "User interaction is not allowed"},
		{"existing empty item", `{"records":{}}`, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetKeychainShot()
			t.Cleanup(resetKeychainShot)
			calls := 0
			err := Open(context.Background(), Store{
				Run: func(context.Context, string, ...string) ([]byte, error) {
					calls++
					return []byte(tc.output), tc.runErr
				},
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Open: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Open error = %v, want %q", err, tc.wantErr)
			}
			if tc.runErr == denied && !errors.Is(err, denied) {
				t.Fatalf("read error identity lost: %v", err)
			}
			if calls != 1 {
				t.Fatalf("Keychain runner calls = %d, want one read", calls)
			}
		})
	}
}
