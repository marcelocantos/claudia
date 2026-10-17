// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
)

// Rewind used to roll seat name back by n user turns and relaunch it
// (🎯T75.8). A seat held by a claudia daemon still asks the daemon, and
// the returned Agent is the same handle. Every other seat refuses before
// the lifecycle reservation, before Stop, and before the transcript is
// truncated. Capability checks run first.
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
	return nil, nil, fmt.Errorf("%w: rewind", ErrLegacyCLISeatTransport)
}

// registryRewindJSONL is the truncation Registry.Rewind performs; tests hold
// it open to race a Launch against it.
var registryRewindJSONL = rewindJSONL
