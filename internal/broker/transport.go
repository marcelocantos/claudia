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
	"time"
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
	// MaxLineLen bounds one wire line. Framing on a delimiter means a peer that
	// never sends the delimiter would otherwise grow the buffer without limit,
	// so the bound is what stops a stuck or hostile writer from exhausting the
	// broker's memory. Exceeding it is a reported error, never a truncation:
	// half a JSON message that happens to parse is the worst outcome available.
	//
	// It is exported because the producers that fill a frame — the event and
	// task-event payload codecs in package claudia — have to size what they
	// relay against the cap the wire actually enforces. A payload budget
	// written as its own literal is a budget that drifts from the limit it
	// exists to respect.
	MaxLineLen = 1 << 20
	// lineReadBuf is the per-connection read buffer. Real messages are a few
	// hundred bytes; a longer line is read in as many buffer-fulls as it takes,
	// so this trades memory per connection against read calls per long line and
	// never bounds a message on its own.
	lineReadBuf = 64 << 10
)

// ErrFrameTooLarge is the named cause behind every oversized-frame refusal, in
// both directions: a peer that sent a line over MaxLineLen, and a message this
// process declined to write because it would have been one.
//
// It is what callers match on. The broker multiplexes every response and push
// for a connection onto that one connection, so the difference between
// dropping a frame and dropping the connection is the difference between one
// screenshot going missing and every unrelated request in flight dying with it
// (jevons 🎯T661: a 1.2 KB send died behind a 1 MiB PNG). A read loop that
// wants to survive an oversized frame matches this error and keeps reading.
var ErrFrameTooLarge = errors.New("broker: frame exceeds the wire line limit")

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
	br  *bufio.Reader

	mu sync.Mutex
}

// NewConn wraps an established connection in the broker's framing.
func NewConn(c net.Conn) *Conn {
	return &Conn{net: c, br: bufio.NewReaderSize(c, lineReadBuf)}
}

// ReadLine returns the next line, without its terminator. It reports io.EOF
// when the peer closes cleanly, and a *ProtocolError with CodeFrameTooLarge —
// matching ErrFrameTooLarge — when the peer sends a line longer than
// MaxLineLen.
//
// An oversized line is skipped, not fatal. The reader consumes it to its
// terminator before returning, so the next call starts on the next whole
// message and the caller may keep using the connection. This is why the
// framing is a bufio.Reader and not a bufio.Scanner: a Scanner's ErrTooLong is
// terminal, and every subsequent Scan returns it forever, which leaves a
// caller the choice between closing the connection and spinning.
//
// The returned slice is a copy. The read buffer is reused by the next call,
// and a caller that held on to it would watch its message change under it — a
// bug that only appears under load, which is the worst kind to ship.
func (c *Conn) ReadLine() ([]byte, error) {
	var line []byte
	// dropped counts the bytes of a line already past the cap. Once it is
	// non-zero the line is being discarded rather than kept, and the count is
	// what the error reports.
	dropped := 0
	for {
		chunk, err := c.br.ReadSlice('\n')
		if err == nil {
			chunk = chunk[:len(chunk)-1] // the terminator is framing, not content
		}
		switch {
		case dropped > 0:
			dropped += len(chunk)
		case len(line)+len(chunk) > MaxLineLen:
			// The line has outgrown the cap. Stop keeping it — a half message
			// that happens to parse is the worst outcome available — but keep
			// reading, because the bytes after it are somebody else's message.
			dropped = len(line) + len(chunk)
			line = nil
		default:
			line = append(line, chunk...)
		}
		switch {
		case err == nil:
			if dropped > 0 {
				return nil, frameTooLarge(dropped)
			}
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			// A peer that died mid-line leaves a fragment. It is delivered as
			// a line — it parses as malformed, which is what it is — and the
			// next call reports the EOF.
			if dropped > 0 {
				return nil, frameTooLarge(dropped)
			}
			if len(line) == 0 {
				return nil, io.EOF
			}
			return line, nil
		default:
			return nil, err
		}
	}
}

// frameTooLarge is the refusal for a frame of n bytes. It is a *ProtocolError
// so the server answers the peer with a typed code, and it unwraps to
// ErrFrameTooLarge so a read loop can match it without depending on the code.
func frameTooLarge(n int) *ProtocolError {
	return &ProtocolError{Code: CodeFrameTooLarge,
		Msg: fmt.Sprintf("frame of %d bytes exceeds the %d-byte line limit; it was dropped and the connection kept open", n, MaxLineLen)}
}

// ReadRequest reads and parses one client → broker message.
func (c *Conn) ReadRequest() (*Request, error) {
	line, err := c.ReadLine()
	if err != nil {
		return nil, err
	}
	return ParseRequest(line)
}

// ReadResponse reads and parses one broker → client message.
func (c *Conn) ReadResponse() (*Response, error) {
	line, err := c.ReadLine()
	if err != nil {
		return nil, err
	}
	return ParseResponse(line)
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

// SetDeadline sets the read and write deadlines on the underlying connection.
func (c *Conn) SetDeadline(t time.Time) error { return c.net.SetDeadline(t) }

// writeLine frames and writes one message.
func (c *Conn) writeLine(line []byte) error {
	if len(line) > MaxLineLen {
		// Refusing here is the other half of skipping an oversized frame on
		// read, and the bound is the same one the reader applies — a message
		// this process will not write must be exactly a message it would not
		// accept, or the two ends disagree about what the wire carries. A
		// peer handed an unframeable line could only drop it; a producer told
		// its payload did not fit can bound it.
		return frameTooLarge(len(line))
	}

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
