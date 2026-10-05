// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "testing"

// A stopped-seat migration whose predecessor was still running adopts it with
// an adopt-only probe, then migrates it. The destination is a new seat and
// must be loaded, not adopted: an inherited AdoptOnly made the sidecar refuse
// it as "loaded seat differs from registered provider" (jevons J16,
// 2026-10-05).
func TestMigrateDestConfigNeverAdoptsTheDestination(t *testing.T) {
	src := Config{Name: "w", Provider: ProviderGrok, SessionID: "s", AdoptOnly: true, RequireResume: true}
	dst := migrateDestConfig(src, &MigrateArgs{Provider: Provider("anthropic"), Model: "m"})
	if dst.AdoptOnly {
		t.Fatal("migration destination inherited AdoptOnly from an adopted source")
	}
	if dst.Provider != "anthropic" || dst.SessionID != "" || dst.RequireResume || dst.Model != "m" || dst.Name != "w" {
		t.Fatalf("destination config = %+v", dst)
	}
}
