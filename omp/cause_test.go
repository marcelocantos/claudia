// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import "testing"

func TestClassifyCause(t *testing.T) {
	cases := []struct {
		text, cause string
	}{
		{"hello owner", CauseOwner},
		{"[claudia] The host restarted at 2026-09-26T01:37:34Z. continue", CauseRestartNudge},
		{"[event: sentinel] T219 repair", CauseSentinel},
		{"**Impatience incident closed — jevons**", CauseImpatience},
		{"[Agent mm2-t65-keys-doors responded]\nI'll inspect", CauseAgentForward},
		{"[event: rsi-coach] judgment", CauseRSI},
		{"[event: capacity] daily budget", CauseCapacity},
	}
	for _, tc := range cases {
		cause, detail := ClassifyCause(tc.text)
		if cause != tc.cause {
			t.Fatalf("%q cause = %s, want %s", tc.text, cause, tc.cause)
		}
		if stringsContainsNL(detail) {
			t.Fatalf("detail is more than one line: %q", detail)
		}
	}
	_, detail := ClassifyCause("[event: sentinel] first\nsecond line of the whole thread")
	if detail != "[event: sentinel] first" {
		t.Fatalf("detail = %q", detail)
	}
}

func stringsContainsNL(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' {
			return true
		}
	}
	return false
}
