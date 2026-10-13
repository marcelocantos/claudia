// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pimp, 2026-10-13: --pick remaining chose cursor while the served sidecar
// had no Cursor exec handlers, so the seat's tools all answered "Tool not
// available". A sidecar without cursorexec.ts makes cursor unlaunchable on
// the session surface, with reason seat_tools_unwired.
func TestCursorSeatExcludedWhenSidecarLacksExecHandlers(t *testing.T) {
	bare := t.TempDir()
	t.Setenv("CLAUDIA_OMP_SERVER", filepath.Join(bare, "server.ts"))
	err := ProviderBinaryError(ProviderCursor, true)
	if err == nil || !strings.HasPrefix(err.Error(), FleetReasonSeatToolsUnwired) {
		t.Fatalf("cursor session on a bare sidecar: err = %v, want %s", err, FleetReasonSeatToolsUnwired)
	}
	if FleetExclusionReason(err.Error()) != FleetReasonSeatToolsUnwired {
		t.Fatalf("reason = %q", FleetExclusionReason(err.Error()))
	}
	snap := ExcludeUnlaunchable(FleetUsageSnapshot{Providers: []FleetUsageRow{{Provider: ProviderCursor, Admit: true}}},
		map[Provider]string{ProviderCursor: err.Error()})
	if row := snap.Providers[0]; row.Admit || !strings.HasPrefix(row.Reason, FleetReasonSeatToolsUnwired+":") {
		t.Fatalf("roster row = %+v, want admit=no reason %s", row, FleetReasonSeatToolsUnwired)
	}
	if err := ProviderBinaryError(ProviderGrok, true); err != nil {
		t.Fatalf("grok session must not depend on cursorexec.ts: %v", err)
	}

	if err := os.WriteFile(filepath.Join(bare, "cursorexec.ts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ProviderBinaryError(ProviderCursor, true); err != nil {
		t.Fatalf("cursor session on a wired sidecar: %v", err)
	}
	if got := FleetExclusionReason("codex executable not found"); got != FleetReasonBinaryNotFound {
		t.Fatalf("binary reason = %q", got)
	}
}
