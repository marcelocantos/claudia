// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/marcelocantos/claudia/internal/broker"
)

// SyncBrokerSeatProtectedPaths pushes host protected-path policy to the
// production broker for a live seat (jevons 🎯T1145). Jevons keeps
// protected_paths on ~/.jevons/agents.json; the broker registry is
// grants.json and must be updated separately from stitch.
func SyncBrokerSeatProtectedPaths(name string, paths []ProtectedPathRule) error {
	if !usingBroker() {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	paths = slices.Clone(paths)
	client, err := dialBroker()
	if err != nil {
		return err
	}
	defer client.Close()
	raw, err := EncodeGrantDefinition(GrantDefinition{
		AgentDef: AgentDef{Name: name, ProtectedPaths: paths},
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), brokerOpTimeout)
	defer cancel()
	_, err = client.call(ctx, &broker.Request{
		Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{
			Name: name, Def: raw, Adopt: true, Fallback: true,
		},
	})
	if err != nil {
		return fmt.Errorf("sync broker protected_paths for %q: %w", name, err)
	}
	return nil
}
