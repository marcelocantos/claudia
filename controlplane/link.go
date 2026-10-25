// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe/transport"
)

// maxFrame bounds one peer message on the control socket. A diff-sync
// changeset for this pair is far smaller; a larger length is a corrupt or
// hostile stream, not a big table.
const maxFrame = 64 << 20

// linkSide is one process's half of a peer link: its own file, the lock
// every writer of that file takes, and the signal that a local write may
// have something to send.
type linkSide struct {
	lock   sync.Locker
	db     func() *sqlpipe.Database // nil once the file is closed
	flush  <-chan struct{}
	config sqlpipe.PeerConfig
	// client opens the sqlpipe handshake. The broker is the client: it
	// claims the Claudia tables, and the consumer approves the claim.
	client bool
	// state runs under the database lock after successful receives.
	state func(sqlpipe.PeerState) error
}

// runLink runs one sqlpipe peer over conn until conn fails or done closes.
// Each process applies the other's changesets to its own file. Neither ever
// opens the other's file. A fresh link starts with sqlpipe's handshake diff,
// so rows committed while no link was up are delivered on the next one.
// A link stopped by done returns nil: closing conn is how done stops it, so
// whatever read or write that close interrupts is the stop, not a failure.
func runLink(conn net.Conn, done <-chan struct{}, side linkSide) (err error) {
	defer func() {
		select {
		case <-done:
			err = nil
		default:
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	watcher := make(chan struct{})
	go func() {
		defer close(watcher)
		select {
		case <-done:
			cancel()
			_ = conn.Close()
		case <-stopped:
		}
	}()
	defer func() { cancel(); _ = conn.Close(); close(stopped); <-watcher }()

	var peer *sqlpipe.Peer
	if err := locked(side, func(db *sqlpipe.Database) (err error) {
		peer, err = sqlpipe.NewPeer(db, side.config)
		return err
	}); err != nil {
		return err
	}
	defer func() {
		side.lock.Lock()
		_ = peer.Close()
		side.lock.Unlock()
	}()

	link := transport.NewLink(&frameTransport{conn: conn, ctx: ctx})
	link.OwnerLock = side.lock
	link.OnPeerUpdate = side.state
	err = link.RunPeer(ctx, peer, side.client, side.flush, nil)

	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// Both delivery hints share this ordered Unix stream, preserving the existing
// framing. Cancellation closes the connection, including when RunPeer cancels
// its private receiver context after an error or a failed initialization hook.
type frameTransport struct {
	conn net.Conn
	ctx  context.Context
}

func (t *frameTransport) Send(_ context.Context, data []byte) error { return writeFrame(t.conn, data) }
func (t *frameTransport) SendDatagram(data []byte) error            { return t.Send(t.ctx, data) }
func (t *frameTransport) Recv(ctx context.Context) ([]byte, error) {
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = t.conn.Close(); close(closed) })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	return readFrame(t.conn)
}
func (*frameTransport) RecvDatagram(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func locked(side linkSide, fn func(*sqlpipe.Database) error) error {
	side.lock.Lock()
	defer side.lock.Unlock()
	db := side.db()
	if db == nil {
		return fmt.Errorf("control plane: closed")
	}
	return fn(db)
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxFrame {
		return nil, fmt.Errorf("control plane: peer frame of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func writeFrame(w io.Writer, b []byte) error {
	if len(b) == 0 || len(b) > maxFrame {
		return fmt.Errorf("control plane: peer frame of %d bytes", len(b))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(append(hdr[:], b...)); err != nil {
		return err
	}
	return nil
}
