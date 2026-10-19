// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"sync"

	"github.com/marcelocantos/claudia/internal/broker"
)

// ClaudiaDBFile is the broker's own control-plane file name inside its
// state directory. Only the broker writes it; a consumer keeps its own file
// and syncs with the broker over a sqlpipe link (controlplane.Replica).
const ClaudiaDBFile = "claudia-control.db"

// controlReconcile is installed by the controlplane package's init. The
// daemon calls it and does not import that package: controlplane already
// imports this one, and the daemon may import only this library and the
// broker wire.
var (
	controlReconcileMu sync.Mutex
	controlReconcile   func(ctx context.Context, path string, reg *Registry) error
	controlPublishMu   sync.Mutex
	controlPublish     func(path, socket string) (func() error, error)
)

// ControlSocketPath is where the broker serving brokerSocket answers the
// consumer's control-plane sqlpipe link: beside that socket, so a consumer
// that can reach the broker can reach it, and two brokers on one machine
// never share one.
func ControlSocketPath(brokerSocket string) string { return brokerSocket + ".control" }

// ControlSocket is the control socket of the broker this process would
// dial (CLAUDIA_BROKER_SOCKET, else the broker socket in the claudia state
// directory). A consumer passes it to controlplane.Replica.Link.
func ControlSocket() (string, error) {
	p, err := broker.SocketPath()
	if err != nil {
		return "", err
	}
	return ControlSocketPath(p), nil
}

// SetReconcileControlFile installs the startup reconciler. A nil function
// clears it. The controlplane package registers ReconcileFile from init.
func SetReconcileControlFile(fn func(ctx context.Context, path string, reg *Registry) error) {
	controlReconcileMu.Lock()
	controlReconcile = fn
	controlReconcileMu.Unlock()
}

// SetPublishControlFile installs the startup publisher. A nil function
// clears it. The controlplane package registers PublishFile from init.
// PublishFile returns the function that unbinds the seat pump and closes
// the plane.
func SetPublishControlFile(fn func(path, socket string) (func() error, error)) {
	controlPublishMu.Lock()
	controlPublish = fn
	controlPublishMu.Unlock()
}

// OpenControlPublisher opens path as this broker's own control file
// (created when absent), binds the seat pump to it and serves the
// consumer's sqlpipe link on socket (ControlSocketPath). Only the broker daemon process may
// call it: anywhere else it refuses, as it does a second owner of one file.
// No linked publisher is a wiring error.
func OpenControlPublisher(path, socket string) (func() error, error) {
	controlPublishMu.Lock()
	fn := controlPublish
	controlPublishMu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("control plane: publisher not linked")
	}
	return fn(path, socket)
}

// ReconcileControlFile applies desired rows already stored at path, through
// the connection OpenControlPublisher opened; call that first. No linked
// reconciler is a wiring error.
func (r *Registry) ReconcileControlFile(ctx context.Context, path string) error {
	if r == nil {
		return fmt.Errorf("control plane: nil registry")
	}
	controlReconcileMu.Lock()
	fn := controlReconcile
	controlReconcileMu.Unlock()
	if fn == nil {
		return fmt.Errorf("control plane: reconciler not linked")
	}
	return fn(ctx, path, r)
}
