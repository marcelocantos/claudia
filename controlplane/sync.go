// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"fmt"

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

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

// SyncLocked replicates both directions between an open Claudia file and an
// open Jevons file. The caller keeps the databases open. Startup and reconnect
// deliver committed rows during the handshake and the flush that follows it.
func SyncLocked(claudiaDB, jevonsDB *sqlpipe.Database) error {
	if claudiaDB == nil || jevonsDB == nil {
		return fmt.Errorf("control plane: nil database")
	}
	if err := EnsureSchema(claudiaDB); err != nil {
		return err
	}
	if err := EnsureSchema(jevonsDB); err != nil {
		return err
	}
	client, err := sqlpipe.NewPeer(claudiaDB, sqlpipe.PeerConfig{OwnedTables: ClaudiaTables()})
	if err != nil {
		return err
	}
	defer client.Close()
	server, err := sqlpipe.NewPeer(jevonsDB, sqlpipe.PeerConfig{ApproveOwnership: ApproveClaudia})
	if err != nil {
		return err
	}
	defer server.Close()
	if err := sqlpipe.SyncPeerHandshake(client, server); err != nil {
		return err
	}
	if client.State() != sqlpipe.PeerLive || server.State() != sqlpipe.PeerLive {
		return fmt.Errorf("control plane handshake not live: claudia=%d jevons=%d", client.State(), server.State())
	}
	return exchange(client, server)
}

func exchange(client, server *sqlpipe.Peer) error {
	for range 8 {
		n, err := flushInto(client, server)
		if err != nil {
			return err
		}
		m, err := flushInto(server, client)
		if err != nil {
			return err
		}
		if n == 0 && m == 0 {
			return nil
		}
	}
	return fmt.Errorf("control plane: sync did not converge")
}

func flushInto(from, to *sqlpipe.Peer) (int, error) {
	msgs, err := from.Flush()
	if err != nil {
		return 0, err
	}
	for _, om := range msgs {
		if _, err := to.HandleMessage(om.Msg); err != nil {
			return 0, err
		}
	}
	return len(msgs), nil
}
