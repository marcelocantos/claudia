// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"slices"
)

// Whoever starts an agent chooses its tools from its own (🎯T249), so an agent
// can do at most what its creator can, down the whole line of creation, which
// is the line authority follows too. Which tools suit which role is the
// agents' call. The owner holds every tool.

// allFleetTools is every tool there is, which is what the owner holds.
func allFleetTools() []string {
	names := make([]string, len(fleetTools))
	for i, t := range fleetTools {
		names[i] = t.Name
	}
	return names
}

// toolSet reads a saved set, where nil means every tool.
func toolSet(names []string) []string {
	if names == nil {
		return allFleetTools()
	}
	return names
}

// chooseTools checks a requested set against the starter's own. Nil asks for
// all of it.
func chooseTools(requested, may []string) ([]string, error) {
	if requested == nil {
		return slices.Clone(may), nil
	}
	chosen := []string{}
	for _, name := range requested {
		if !slices.Contains(allFleetTools(), name) {
			return nil, fmt.Errorf("there is no tool %q", name)
		}
		if !slices.Contains(may, name) {
			return nil, fmt.Errorf("cannot give tool %q: you do not have it", name)
		}
		if !slices.Contains(chosen, name) {
			chosen = append(chosen, name)
		}
	}
	return chosen, nil
}

// holdsTool reports whether an agent may call a tool.
func (k *FleetKernel) holdsTool(fleet, caller, tool string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	group := k.state.Fleets[fleet]
	if group == nil || group.Agents[caller] == nil {
		return false
	}
	return slices.Contains(toolSet(group.Agents[caller].Tools), tool)
}

// toolsOf is the tool list an agent's MCP client sees.
func (k *FleetKernel) toolsOf(fleet, caller string) []fleetTool {
	out := []fleetTool{}
	for _, t := range fleetTools {
		if k.holdsTool(fleet, caller, t.Name) {
			out = append(out, t)
		}
	}
	return out
}
