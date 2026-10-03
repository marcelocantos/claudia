// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// Pin the actual CLI boundary, not a second provider map in a fake harness.
// If literal subscription IDs become supported, the T171 smoke preflight must
// be revisited alongside the missing behavioral evidence, without aliases.
func TestT171LiteralSubscriptionProviderBoundary(t *testing.T) {
	for _, id := range []string{omp.Anthropic, omp.OpenAICodex, omp.XAIOAuth} {
		if err := checkProvider(id); err == nil {
			t.Fatalf("%s now supported: revisit the T171 live preflight", id)
		}
	}
	if err := checkProvider(omp.Cursor); err != nil {
		t.Fatal(err)
	}
}
