// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"sync"
)

// ClaudiaDBFile is the Claudia-hosted control-plane file name inside a
// state directory. The daemon reconciles it before the broker socket listens.
const ClaudiaDBFile = "claudia-control.db"

// controlReconcile is installed by the controlplane package's init. The
// daemon calls it and does not import that package: controlplane already
// imports this one, and the daemon may import only this library and the
// broker wire.
var (
	controlReconcileMu sync.Mutex
	controlReconcile   func(ctx context.Context, path string, reg *Registry) error
	controlPublishMu   sync.Mutex
	controlPublish     func(path string) (func() error, error)
)

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
func SetPublishControlFile(fn func(path string) (func() error, error)) {
	controlPublishMu.Lock()
	controlPublish = fn
	controlPublishMu.Unlock()
}

// OpenControlPublisher binds the seat pump to an existing control file.
// A missing file returns a nil closer and is not created. No linked
// publisher is a wiring error, not a missing file.
func OpenControlPublisher(path string) (func() error, error) {
	controlPublishMu.Lock()
	fn := controlPublish
	controlPublishMu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("control plane: publisher not linked")
	}
	return fn(path)
}

// ReconcileControlFile applies desired rows already stored at path.
// The linked reconciler treats a missing file as success. No linked
// reconciler is a wiring error, not a missing file.
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
