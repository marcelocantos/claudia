// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// GapTestAgent wires QueryUsageDirect for hermetic T1398 CP/cursor integration tests.
func GapTestAgent(t *testing.T, seat, session, socket string, cursor uint64) *Agent {
	t.Helper()
	t.Setenv(omp.SocketEnv, socket)
	a := &Agent{sessionID: session, startCfg: Config{Name: seat}, usageCursor: cursor}
	a.ops.usagePage = func(ctx context.Context, _ *Agent, after uint64, limit int) (*UsagePage, error) {
		return QueryUsageDirect(ctx, UsageQuery{Seat: seat, SessionID: session, After: after, Limit: limit})
	}
	return a
}

// GapTestUsageCursor reports the refresh projection offset (usageCursor).
func (a *Agent) GapTestUsageCursor() uint64 {
	if a == nil {
		return 0
	}
	return a.usageCursor
}
