// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

// ApproveClaudia accepts a peer that claims only Claudia-owned tables.
// An empty claim and a Jevons-owned table are rejected.
func ApproveClaudia(tables map[string]bool) bool {
	if len(tables) == 0 {
		return false
	}
	for name := range tables {
		if !Owns(name) {
			return false
		}
	}
	return true
}
