// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// Replica is a consumer's own control file (Jevons: JevonsDBFile in its own
// state directory). The consumer writes the Jevons tables here and only
// here. It receives the Claudia tables from the broker over Link, and the
// broker receives the Jevons tables the same way. The consumer never opens
// the broker's claudia-control.db.
type Replica struct {
	mu    sync.Mutex
	db    *sqlpipe.Database
	flush chan struct{}
}

// OpenReplica opens or creates the consumer's file at path. A path named
// ClaudiaDBFile is refused: that file is the broker's.
func OpenReplica(path string) (*Replica, error) {
	if filepath.Base(path) == ClaudiaDBFile {
		return nil, fmt.Errorf("%w: %s is the broker's file; a consumer keeps its own (%s)", ErrNotOwner, path, JevonsDBFile)
	}
	db, err := openFile(path)
	if err != nil {
		return nil, err
	}
	return &Replica{db: db, flush: make(chan struct{}, 1)}, nil
}

// Write runs fn against the consumer's file while the link waits, then has
// a live link send what fn committed. The Jevons Publish functions
// (PublishDesiredSeat, PublishGrant, PublishControlIntent) are what fn calls.
func (r *Replica) Write(fn func(*sqlpipe.Database) error) error {
	r.mu.Lock()
	if r.db == nil {
		r.mu.Unlock()
		return fmt.Errorf("control plane: replica closed")
	}
	err := fn(r.db)
	r.mu.Unlock()
	select {
	case r.flush <- struct{}{}:
	default:
	}
	return err
}

// Read runs fn against the consumer's file while the link waits. The
// Claudia tables read here are what the broker last delivered.
func (r *Replica) Read(fn func(*sqlpipe.Database) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return fmt.Errorf("control plane: replica closed")
	}
	return fn(r.db)
}

// Link dials the broker's control socket (claudia.ControlSocketPath of the
// broker socket) and keeps the two files in sync until ctx ends or the
// broker goes away. It returns then. A caller that wants the link kept up
// dials again, and the reconnect's handshake delivers whatever either side
// committed while the link was down.
func (r *Replica) Link(ctx context.Context, socket string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopped before the link was up
		}
		return fmt.Errorf("control plane: dial broker: %w", err)
	}
	return runLink(conn, ctx.Done(), linkSide{
		lock:   &r.mu,
		db:     func() *sqlpipe.Database { return r.db },
		flush:  r.flush,
		config: sqlpipe.PeerConfig{ApproveOwnership: ApproveClaudia},
	})
}

// Close releases the consumer's file.
func (r *Replica) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}
