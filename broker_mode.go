// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"

	"github.com/marcelocantos/claudia/internal/broker"
)

// The library consult (🎯T3). Start and Task.Run ask usingBroker before any
// socket work so CLAUDIA_NO_BROKER=1 is a live escape hatch: no filesystem
// or network syscall in between. When a daemon answers, the seat or the run
// is the daemon's and this process holds a socket client; when nothing
// listens, or a bare protocol server answers CodeNotAvailable, the direct
// path starts the agent exactly as it did before the broker existed.

// brokerFellThrough reports the two errors that mean "no daemon here" and
// therefore license the direct path. Any other error is the daemon's
// answer and is returned to the caller.
func brokerFellThrough(err error) bool {
	return errors.Is(err, errNoBroker) || errors.Is(err, errBrokerNotAvailable)
}

func startConsideringBroker(cfg Config, backend agentBackend) (*Agent, error) {
	return startConsideringBrokerContext(context.Background(), cfg, backend)
}

func startConsideringBrokerContext(ctx context.Context, cfg Config, backend agentBackend) (*Agent, error) {
	if usingBroker() {
		a, err := startViaBrokerContext(ctx, cfg)
		if err == nil {
			return a, nil
		}
		if !brokerFellThrough(err) {
			return nil, err
		}
	}
	return startWithBackendContext(ctx, cfg, backend)
}

// taskBackendConsideringBroker returns the broker backend for one run when
// a daemon listens, else nil. The caller falls back to direct on
// errBrokerNotAvailable from RunTask.
func taskBackendConsideringBroker(cfg TaskConfig) *brokerTaskBackend {
	if !usingBroker() {
		return nil
	}
	client, err := dialBroker()
	if err != nil {
		return nil
	}
	return &brokerTaskBackend{cfg: cfg, client: client}
}

// brokerSocketPath is the socket a consumer would dial, for diagnostics.
func brokerSocketPath() string {
	p, err := broker.SocketPath()
	if err != nil {
		return ""
	}
	return p
}
