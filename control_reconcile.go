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
)

// SetReconcileControlFile installs the startup reconciler. A nil function
// clears it. The controlplane package registers ReconcileFile from init.
func SetReconcileControlFile(fn func(ctx context.Context, path string, reg *Registry) error) {
	controlReconcileMu.Lock()
	controlReconcile = fn
	controlReconcileMu.Unlock()
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
