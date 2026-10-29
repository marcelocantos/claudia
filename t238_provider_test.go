// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
package claudia

import (
	"github.com/marcelocantos/claudia/omp"
	"strings"
	"testing"
)

func TestT238ReadyProvesActualProviderIncludingOldSidecar(t *testing.T) {
	for _, tc := range []struct {
		name   string
		actual string
		fail   bool
	}{
		{"new provider", "anthropic", false},
		{"old provider still active", "xai-oauth", true},
		{"older sidecar lacks identity", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyOMPReadyProvider("seat", "anthropic", omp.Event{Type: "ready", Provider: tc.actual})
			if tc.fail && (err == nil || !strings.Contains(err.Error(), "provider mismatch")) {
				t.Fatalf("ready from %q: %v", tc.actual, err)
			}
			if !tc.fail && err != nil {
				t.Fatal(err)
			}
		})
	}
}
