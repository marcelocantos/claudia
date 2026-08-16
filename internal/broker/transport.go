// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// The broker's socket transport (🎯T2.1): binding, dialing, and the
// newline-delimited-JSON framing that carries the wire contract in wire.go.
//
// This file carries the transport marker so the policy guard permits the dials
// below. The waiver is narrow on purpose, and this file honours that narrowness:
// it moves bytes and never interprets them. What a line *means* — including a
// 429 — stays with the wire contract and the policy loops, so the shape of a
// rate limit keeps exactly one reader.
//
//claudia:transport

// unixNetwork is the only network the broker speaks. It is deliberately not a
// parameter: a broker reachable over TCP would be a remote-code-execution
// service, since anyone who can connect can start processes as this user.
const unixNetwork = "unix"

const (
	// maxLineLen bounds one wire line. Framing on a delimiter means a peer that
	// never sends the delimiter would otherwise grow the buffer without limit,
	// so the bound is what stops a stuck or hostile writer from exhausting the
	// broker's memory. Exceeding it is a reported error, never a truncation:
	// half a JSON message that happens to parse is the worst outcome available.
	maxLineLen = 1 << 20
	// initialLineBuf is the starting read buffer. Real messages are a few
	// hundred bytes; the buffer grows on demand up to maxLineLen.
	initialLineBuf = 4096
)

// Listen binds the broker's socket at path.
//
// It clears a socket left behind by a crashed broker, but never one a live
// broker still answers on — see clearStaleSocket.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), stateDirPerm); err != nil {
		return nil, fmt.Errorf("broker: create state directory for %s: %w", path, err)
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}
	ln, err := net.Listen(unixNetwork, path)
	if err != nil {
		return nil, fmt.Errorf("broker: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, socketPerm); err != nil {
		// A socket we cannot restrict is one we must not serve: leaving it
		// world-writable would hand the spawn capability to any local user.
		ln.Close()
		return nil, fmt.Errorf("broker: restrict permissions on %s: %w", path, err)
	}
	return ln, nil
}

// clearStaleSocket removes a socket file that no broker is answering on.
//
// It dials before unlinking. A socket that still accepts a connection belongs to
// a running broker, and removing it would silently steal every future client
// from a daemon that keeps running and keeps holding agents — a split brain with
// no error anywhere. That case is a refusal, not a takeover.
func clearStaleSocket(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("broker: inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("broker: %s exists and is not a socket; refusing to remove it", path)
	}
	if c, err := net.Dial(unixNetwork, path); err == nil {
		c.Close()
		return fmt.Errorf("broker: %s is already served by a running broker", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("broker: remove stale socket %s: %w", path, err)
	}
	return nil
}

// Dial connects to a broker listening at path.
func Dial(path string) (*Conn, error) {
	c, err := net.Dial(unixNetwork, path)
	if err != nil {
		return nil, fmt.Errorf("broker: dial %s: %w", path, err)
	}
	return NewConn(c), nil
}

// Conn is one newline-delimited-JSON connection, in either direction.
//
// Writes are serialised: the broker answers requests from its connection
// goroutine while pushing tail events from another, and two interleaved Write
// calls would splice one message into the middle of another. Reads are not
// serialised, because a connection has exactly one reader by construction.
type Conn struct {
	net net.Conn
	sc  *bufio.Scanner

	mu sync.Mutex
}

// NewConn wraps an established connection in the broker's framing.
func NewConn(c net.Conn) *Conn {
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, initialLineBuf), maxLineLen)
	return &Conn{net: c, sc: sc}
}

// ReadLine returns the next line, without its terminator. It reports io.EOF when
// the peer closes cleanly, and a *ProtocolError with CodeMalformed when the peer
// sends a line longer than maxLineLen.
//
// The returned slice is a copy. The scanner's own buffer is reused by the next
// read, and a caller that held on to it would watch its message change under it
// — a bug that only appears under load, which is the worst kind to ship.
func (c *Conn) ReadLine() ([]byte, error) {
	if c.sc.Scan() {
		line := c.sc.Bytes()
		out := make([]byte, len(line))
		copy(out, line)
		return out, nil
	}
	err := c.sc.Err()
	switch {
	case err == nil:
		return nil, io.EOF
	case errors.Is(err, bufio.ErrTooLong):
		return nil, &ProtocolError{Code: CodeMalformed,
			Msg: fmt.Sprintf("message exceeds the %d-byte line limit", maxLineLen)}
	default:
		return nil, err
	}
}

// WriteRequest encodes and sends one client → broker message.
func (c *Conn) WriteRequest(r *Request) error {
	line, err := r.Encode()
	if err != nil {
		return err
	}
	return c.writeLine(line)
}

// WriteResponse encodes and sends one broker → client message.
func (c *Conn) WriteResponse(r *Response) error {
	line, err := r.Encode()
	if err != nil {
		return err
	}
	return c.writeLine(line)
}

// writeLine frames and writes one message.
func (c *Conn) writeLine(line []byte) error {
	// The terminator is appended into one buffer rather than written
	// separately, so a peer that dies mid-message cannot leave a line the
	// receiver will glue to the next one.
	framed := make([]byte, 0, len(line)+1)
	framed = append(framed, line...)
	framed = append(framed, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.net.Write(framed); err != nil {
		return fmt.Errorf("broker: write %d-byte message: %w", len(line), err)
	}
	return nil
}

// Close closes the underlying connection.
func (c *Conn) Close() error { return c.net.Close() }
