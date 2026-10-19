// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
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
}

// runLink runs one sqlpipe peer over conn until conn fails or done closes.
// Each process applies the other's changesets to its own file. Neither ever
// opens the other's file. A fresh link starts with sqlpipe's handshake diff,
// so rows committed while no link was up are delivered on the next one.
func runLink(conn net.Conn, done <-chan struct{}, side linkSide) error {
	defer conn.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-done:
			_ = conn.Close()
		case <-stop:
		}
	}()

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

	frames := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		for {
			b, err := readFrame(conn)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- b:
			case <-stop:
				return
			}
		}
	}()
	send := func(msgs []sqlpipe.PeerOutMessage) error {
		for _, m := range msgs {
			if err := writeFrame(conn, sqlpipe.SerializePeer(m.Msg)); err != nil {
				return err
			}
		}
		return nil
	}

	if side.client {
		var out []sqlpipe.PeerOutMessage
		if err := locked(side, func(*sqlpipe.Database) (err error) {
			out, err = peer.Start()
			return err
		}); err != nil {
			return err
		}
		if err := send(out); err != nil {
			return err
		}
	}
	// pending holds a local write's flush signal that arrived before the
	// handshake finished; it is sent as soon as the link is live.
	pending := true
	flushIfLive := func() error {
		var out []sqlpipe.PeerOutMessage
		if err := locked(side, func(*sqlpipe.Database) (err error) {
			if peer.State() != sqlpipe.PeerLive {
				return nil
			}
			pending = false
			out, err = peer.Flush()
			return err
		}); err != nil {
			return err
		}
		return send(out)
	}
	for {
		select {
		case <-done:
			return nil
		case err := <-readErr:
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		case b := <-frames:
			msg, err := sqlpipe.DeserializePeer(b)
			if err != nil {
				return err
			}
			var hr sqlpipe.PeerHandleResult
			if err := locked(side, func(*sqlpipe.Database) (err error) {
				hr, err = peer.HandleMessage(msg)
				return err
			}); err != nil {
				return err
			}
			if err := send(hr.Messages); err != nil {
				return err
			}
			if pending {
				if err := flushIfLive(); err != nil {
					return err
				}
			}
		case <-side.flush:
			pending = true
			if err := flushIfLive(); err != nil {
				return err
			}
		}
	}
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
