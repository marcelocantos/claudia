// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

// TestMain keeps the hermetic suite off any claudia daemon installed on the
// machine. Start, Task.Run and LoadPlanUsage consult the socket when one
// exists (🎯T3); without this a developer's live daemon would be granted
// every fixture seat the suite starts, with real provider processes behind
// them. Tests that want a daemon start their own on a temp socket and
// re-enable the consult with t.Setenv(broker.NoBrokerEnv, "").
func TestMain(m *testing.M) {
	if os.Getenv(broker.NoBrokerEnv) == "" {
		_ = os.Setenv(broker.NoBrokerEnv, "1")
	}
	os.Exit(m.Run())
}
