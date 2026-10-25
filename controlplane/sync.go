// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import "github.com/marcelocantos/sqlpipe/go/sqlpipe"

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

// pairTables excludes private local tables from schema negotiation and ownership.
func pairTables() *sqlpipe.TableFilter {
	tables := ClaudiaTables()
	for _, table := range []string{DesiredSeatTable, GrantTable, IntentTable} {
		tables[table] = true
	}
	return &sqlpipe.TableFilter{Tables: tables}
}
