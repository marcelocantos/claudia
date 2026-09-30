// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T165: on 2026-10-01 the overseer and its PO "refused" their token every
// few seconds. Their conversations held an earlier 401, and their replies
// discussed it; the detector read the whole snapshot and the reply text, so
// every turn end renewed the plan's token under the whole fleet. Only the
// turn's own error says whether the token was refused.
func TestT165OldAuthErrorInHistoryIsNotARefusal(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "live", &refreshes, "renewed")
	agent, conn := startT141Seat(t, s)

	history := `{"messages":[` +
		`{"role":"assistant","stopReason":"error","errorMessage":"401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"OAuth access token has been revoked.\"}}"},` +
		`{"role":"user"},` +
		`{"role":"assistant","stopReason":"stop"}]}`
	t141Write(t, conn, `{"type":"turn_end","text":"The 401 authentication_error came from a stale token.","snapshot":`+history+`}`)
	s.settle(t, agent, conn)
	if n := refreshes.Load(); n != 0 {
		t.Fatalf("a normal turn whose history holds an old 401 renewed the token %d times", n)
	}

	// The turn's own refusal still counts.
	refused := `{"messages":[{"role":"assistant","stopReason":"error","errorMessage":"401 authentication_error: OAuth access token has been revoked."}]}`
	t141Write(t, conn, `{"type":"turn_end","snapshot":`+refused+`}`)
	for m := range s.got { // `go test -timeout` is the clock
		if m.Op == omp.OpToken {
			if m.Token != "renewed" {
				t.Fatalf("a turn refused on its token was not recovered: %+v", m)
			}
			break
		}
	}
	if n := refreshes.Load(); n != 1 {
		t.Fatalf("renewals = %d, want 1", n)
	}
}
