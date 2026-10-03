// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestT171DetailedRecoveryBranches(t *testing.T) {
	for _, tc := range []struct {
		name, initial, outcome, class string
		fail, persistFail             bool
	}{
		{"healthy", "live", RecoveryHealthyNoOp, "none", false, false},
		{"refresh", "expired", RecoveryRefreshed, "none", false, false},
		{"refused", "expired", RecoveryFailure, "needs_sign_in", true, false},
		{"persist", "expired", RecoveryFailure, "persistence_failed", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiry := time.Now().Add(-time.Hour)
			if tc.initial == "live" {
				expiry = time.Now().Add(time.Hour)
			}
			store := t165Store(t, tc.initial, expiry)
			if tc.persistFail {
				parent := filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(parent, []byte("block"), 0600); err != nil {
					t.Fatal(err)
				}
				store.DataPath = filepath.Join(parent, "plan.enc")
			}
			var verbs []string
			login := t165Login(&verbs, func() ([]byte, error) {
				if tc.fail {
					return nil, errors.New("invalid_grant SECRET_HELPER_OUTPUT")
				}
				return []byte(`{"refresh_token":"SECRET_REFRESH","access_token":"SECRET_ACCESS","expiry":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
			})
			// Detailed recovery cannot inherit an accidental interactive setting.
			login.ForceLogin = true
			result := RecoverPlanNoLogin(context.Background(), store, login, Anthropic)
			if result.Outcome != tc.outcome || result.Classification != tc.class {
				t.Fatalf("result=%+v", result)
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "SECRET") {
				t.Fatal("recovery leaked helper/credential material")
			}
			if tc.initial == "live" {
				if len(verbs) != 0 {
					t.Fatalf("healthy no-op ran %v", verbs)
				}
			} else if !reflect.DeepEqual(verbs, []string{"refresh"}) {
				t.Fatalf("unattended recovery ran %v", verbs)
			}
			if result.Outcome == RecoveryRefreshed {
				item, err := readDataFile(store.DataPath, shot.key)
				if err != nil || item.Records[Anthropic].AccessToken != "SECRET_ACCESS" {
					t.Fatal("reported refresh was not persisted", err)
				}
			}
		})
	}
}
