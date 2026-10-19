// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// stallConn is a connection whose writes block until it is closed, as a
// peer that stopped reading would make them. Close is what runLink does when
// its caller's context ends.
type stallConn struct {
	net.Conn
	once    sync.Once
	closed  chan struct{}
	writing chan struct{}
}

func (c *stallConn) Write([]byte) (int, error) {
	select {
	case c.writing <- struct{}{}:
	default:
	}
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stallConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// A link its caller stops is not a failed link. runLink is sending when
// done closes; the write fails because done closed the connection, and that
// is the caller's stop, which Link reports as a clean return. Before the
// fix it returned "use of closed network connection", which made
// TestConcurrentSeatPumpWritesAndHandshakesLoseNothing flaky.
func TestLinkStoppedMidSendReturnsCleanly(t *testing.T) {
	r, err := OpenReplica(filepath.Join(t.TempDir(), JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	conn := &stallConn{closed: make(chan struct{}), writing: make(chan struct{}, 1)}
	done := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- runLink(conn, done, linkSide{
			lock:   &r.mu,
			db:     func() *sqlpipe.Database { return r.db },
			flush:  r.flush,
			config: sqlpipe.PeerConfig{OwnedTables: ClaudiaTables()},
			client: true, // sends its hello first
		})
	}()
	<-conn.writing
	close(done)
	if err := <-ended; err != nil {
		t.Fatalf("a link stopped by its caller returned %v, want nil", err)
	}
}

// Stopping Link before its dial completes is the same caller stop.
func TestLinkStoppedMidDialReturnsCleanly(t *testing.T) {
	r, err := OpenReplica(filepath.Join(t.TempDir(), JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, sock := brokerDirs(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.Link(ctx, sock); err != nil {
		t.Fatalf("a link stopped before it dialled returned %v, want nil", err)
	}
}
