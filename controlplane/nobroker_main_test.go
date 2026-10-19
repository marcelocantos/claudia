// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"os"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

// TestMain keeps this package's tests off any claudia broker running on the
// machine: this test binary links the broker client, and the library
// consults the default socket whenever one listens (🎯T3). A test that wants
// a broker starts its own on a temp socket and re-enables the consult with
// t.Setenv(broker.NoBrokerEnv, ""). internal/brokerguard ratchets this.
func TestMain(m *testing.M) {
	if os.Getenv(broker.NoBrokerEnv) == "" {
		_ = os.Setenv(broker.NoBrokerEnv, "1")
	}
	os.Exit(m.Run())
}
