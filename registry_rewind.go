// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"path/filepath"
)

// Rewind rolls seat name back by n user turns and relaunches it on the
// rewound conversation (🎯T75.8). It is [Agent.Rewind] for a seat the
// Registry holds: the stop, the truncation and the relaunch happen under one
// reservation, so no concurrent Launch can restart the seat on the old
// transcript in between, and the Registry keeps the relaunched handle. A
// concurrent [Registry.Stop] still wins and cancels the relaunch.
//
// When the seat is held by a claudia daemon, the daemon does this and the
// returned Agent is the same handle, re-pointed, with its subscriptions.
//
// If the transcript cannot be truncated the seat is relaunched unchanged and
// the error is returned with the handle, so a failed rewind does not also
// leave the seat down.
func (r *Registry) Rewind(ctx context.Context, name string, n int) (*Agent, *RewindResult, error) {
	def := r.Def(name)
	if def == nil {
		return nil, nil, fmt.Errorf("agent %q not registered", name)
	}
	if err := CheckCapability(def.Provider, CapabilityRewind); err != nil {
		return nil, nil, err
	}
	if proc := r.Get(name); proc != nil && proc.brokerGrant != "" {
		res, err := proc.rewindOnBroker(n)
		if err != nil {
			return proc, nil, err
		}
		return proc, res, nil
	}

	ctx, op, finish, err := r.beginLifecycle(ctx, name, false)
	if err != nil {
		return nil, nil, err
	}
	defer finish()
	jsonlPath := SessionJSONLPath(def.SessionID, def.WorkDir)
	if proc := r.Get(name); proc != nil && proc.JSONLPath() != "" {
		jsonlPath = proc.JSONLPath()
	} else if resolved, err := filepath.EvalSymlinks(def.WorkDir); err == nil {
		jsonlPath = SessionJSONLPath(def.SessionID, resolved)
	}
	if err := r.stopHeld(name, false); err != nil {
		return nil, nil, err
	}
	res, rewindErr := registryRewindJSONL(jsonlPath, n)
	if res != nil {
		res.SessionID = def.SessionID
	}
	proc, err := r.startHeld(ctx, op, name, false, false)
	if rewindErr != nil {
		return proc, nil, rewindErr
	}
	if err != nil {
		return nil, res, err
	}
	return proc, res, nil
}

// registryRewindJSONL is the truncation Registry.Rewind performs; tests hold
// it open to race a Launch against it.
var registryRewindJSONL = rewindJSONL
