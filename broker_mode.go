// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// The T2.1 library consult. Start and Task.Run ask usingBroker before any
// socket work so CLAUDIA_NO_BROKER=1 is a live escape hatch: no filesystem
// or network syscall in between. The RPC client that turns a listening
// broker into a spawn is 🎯T3; until then a listening broker is probed with
// status so the consult is observable and the fallback path stays the one
// that actually starts the agent.

const brokerProbeTimeout = 2 * time.Second

// usingBroker reports whether this process may talk to a lifecycle broker.
func usingBroker() bool {
	return !broker.Disabled()
}

// considerBroker is the socket half of the consult. When the broker is
// disabled it is never called. When the socket is absent, Dial fails and
// the caller falls through to the direct path.
func considerBroker() {
	path, err := broker.SocketPath()
	if err != nil {
		return
	}
	c, err := broker.Dial(path)
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(brokerProbeTimeout))
	_ = c.WriteRequest(&broker.Request{ID: "probe", Type: broker.TypeStatus, Status: &broker.StatusRequest{}})
	_, _ = c.ReadResponse()
}

func startConsideringBroker(cfg Config, backend agentBackend) (*Agent, error) {
	if usingBroker() {
		considerBroker()
	}
	return startWithBackend(cfg, backend)
}
