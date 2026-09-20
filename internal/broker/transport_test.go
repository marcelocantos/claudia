// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

// Oracles for the oversized-frame contract (🎯T73).
//
// The defect these pin is not "a big line is rejected" — it always was. It is
// what happens next. The broker multiplexes every response and push for a
// consumer onto one connection, and closing that connection over one
// unrelayable frame kills every unrelated request sharing it: jevons 🎯T661
// lost a 1.2 KB send to a screenshot. So each test here checks the survival
// half as well as the refusal, because a fix that reports the oversize and
// still drops the connection has not done the job.

// pipeConn is an in-process connection pair. A Unix socket would work too, but
// these tests write frames of megabytes and care only about framing.
func pipeConn(t *testing.T) (client, server *Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return NewConn(a), NewConn(b)
}

// writeRaw puts bytes on the wire without the framing's own size check, which
// is how a peer that does not share our limits behaves.
//
// Errors go to errc and never to t. These writes run on a goroutine that
// outlives a failing test: an assertion that gives up on the connection leaves
// the peer blocked mid-write, and it wakes only when cleanup closes the pipe —
// by which time touching t panics the whole package with "Fail in goroutine
// after ... has completed" and buries the assertion that actually failed. That
// is not hypothetical. It is what happens under the mutation these tests exist
// to catch, which is the one run where the panic costs the most: the evidence
// that the mutation bites is precisely the failure the panic swallows.
func writeRaw(errc chan<- error, c *Conn, b []byte) {
	if _, err := c.net.Write(b); err != nil {
		select {
		case errc <- err:
		default: // the first error is the one that explains the rest
		}
	}
}

// peerErrs is the channel writeRaw reports on. One slot: a peer that fails a
// write fails every later one too, and the first is the informative one.
func peerErrs() chan error { return make(chan error, 1) }

// noPeerError fails the test if the peer goroutine could not write what the
// test asked it to. It is called while the test is still running, which is the
// whole reason the error travelled on a channel to get here.
func noPeerError(t *testing.T, errc <-chan error) {
	t.Helper()
	select {
	case err := <-errc:
		t.Errorf("raw write: %v", err)
	default:
	}
}

// TestReadLineSkipsAnOversizedFrameAndKeepsReading is the acceptance oracle:
// the frame in the middle is refused by name, and the frame after it — which
// on a real connection is somebody else's answer — still arrives.
func TestReadLineSkipsAnOversizedFrameAndKeepsReading(t *testing.T) {
	// The sizes matter. A frame that overshoots the cap by less than one read
	// buffer is consumed to its terminator by the read that notices — so a
	// reader that reports the oversize and stops consuming would still look
	// correct. A screenshot overshoots by megabytes, and only that case
	// distinguishes skipping the frame from abandoning the stream mid-line.
	for _, over := range []int{7, lineReadBuf + 1, 3 * MaxLineLen} {
		t.Run(fmt.Sprintf("over-by-%d", over), func(t *testing.T) {
			client, server := pipeConn(t)

			oversized := append(bytes.Repeat([]byte("x"), MaxLineLen+over), '\n')
			errc := peerErrs()
			go func() {
				writeRaw(errc, client, []byte("before\n"))
				writeRaw(errc, client, oversized)
				writeRaw(errc, client, []byte("after\n"))
			}()

			line, err := server.ReadLine()
			if err != nil || string(line) != "before" {
				t.Fatalf("first line: %q, %v", line, err)
			}

			line, err = server.ReadLine()
			if err == nil {
				t.Fatalf("oversized frame was accepted as %q", line)
			}
			if !errors.Is(err, ErrFrameTooLarge) {
				t.Fatalf("oversized frame is not errors.Is ErrFrameTooLarge: %v", err)
			}
			if line != nil {
				t.Errorf("a refused frame must yield no content, got %d bytes", len(line))
			}

			// The survival half. Without it this is the 🎯T661 defect with a
			// better error message.
			line, err = server.ReadLine()
			if err != nil || string(line) != "after" {
				t.Fatalf("connection did not survive the oversized frame: %.40q, %v", line, err)
			}
			noPeerError(t, errc)
		})
	}
}

// TestOversizedFrameIsTypedAndSurvivesTheWire pins the error's identity in
// both forms: the Go value the reader raises, and the one a client rebuilds
// from the error message the broker sent back. The code is the identity, so
// errors.Is works at both ends without matching on message text.
func TestOversizedFrameIsTypedAndSurvivesTheWire(t *testing.T) {
	err := frameTooLarge(MaxLineLen + 1)
	var pe *ProtocolError
	if !errors.As(error(err), &pe) || pe.Code != CodeFrameTooLarge {
		t.Fatalf("want a *ProtocolError with %s, got %+v", CodeFrameTooLarge, err)
	}
	if !errors.Is(error(err), ErrFrameTooLarge) {
		t.Fatal("the raised refusal does not match ErrFrameTooLarge")
	}
	if !strings.Contains(err.Msg, fmt.Sprint(MaxLineLen+1)) {
		t.Errorf("the refusal does not name the size it refused: %s", err.Msg)
	}
	// Through the wire body and back.
	if !errors.Is(err.Wire().Err(), ErrFrameTooLarge) {
		t.Fatal("a refusal decoded off the wire does not match ErrFrameTooLarge")
	}
	// The sentinel must not swallow unrelated refusals.
	other := &ProtocolError{Code: CodeMalformed, Msg: "not json"}
	if errors.Is(other, ErrFrameTooLarge) {
		t.Fatal("CodeMalformed matches ErrFrameTooLarge — the sentinel is over-broad")
	}
}

// TestReadLineAcceptsAFrameExactlyAtTheCap keeps the bound from drifting to
// off-by-one. MaxLineLen is the largest legal line, not the first illegal one.
func TestReadLineAcceptsAFrameExactlyAtTheCap(t *testing.T) {
	client, server := pipeConn(t)
	exact := bytes.Repeat([]byte("y"), MaxLineLen)
	errc := peerErrs()
	go func() { writeRaw(errc, client, append(exact, '\n')) }()

	line, err := server.ReadLine()
	if err != nil {
		t.Fatalf("a line of exactly MaxLineLen was refused: %v", err)
	}
	if len(line) != MaxLineLen || !bytes.Equal(line, exact) {
		t.Fatalf("got %d bytes, want %d intact", len(line), MaxLineLen)
	}
	noPeerError(t, errc)
}

// TestReadLineCopiesEachFrame pins the copy the doc comment promises: a caller
// that keeps a line must not watch it change when the next one is read.
func TestReadLineCopiesEachFrame(t *testing.T) {
	client, server := pipeConn(t)
	errc := peerErrs()
	go func() {
		writeRaw(errc, client, []byte("first\n"))
		writeRaw(errc, client, []byte("second\n"))
	}()
	first, err := server.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.ReadLine(); err != nil {
		t.Fatal(err)
	}
	if string(first) != "first" {
		t.Fatalf("the first line changed under the caller: %q", first)
	}
	noPeerError(t, errc)
}

// TestWriteLineRefusesAFrameItCannotFrame is the producer half. A frame this
// process cannot legally write is one the peer could only answer by dropping,
// so the writer learns and the connection stays usable for the next message.
func TestWriteLineRefusesAFrameItCannotFrame(t *testing.T) {
	client, server := pipeConn(t)

	huge := bytes.Repeat([]byte("z"), MaxLineLen+1)
	errc := make(chan error, 1)
	go func() {
		errc <- client.writeLine(huge)
		errc <- client.writeLine([]byte("still here"))
	}()

	if err := <-errc; !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("an unframeable line was written: %v", err)
	}
	// Nothing of the refused frame reached the wire: the next read is the
	// next message, not the tail of the one that was refused.
	line, err := server.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "still here" {
		t.Fatalf("the refused frame leaked onto the wire: %q", line)
	}
	if err := <-errc; err != nil {
		t.Fatalf("the connection did not survive the refusal: %v", err)
	}
}

// TestWriteLineAcceptsExactlyWhatReadLineAccepts pins the two ends against
// each other. A line of exactly MaxLineLen is legal to read, so it must be
// legal to write: a writer stricter than the reader silently forbids messages
// the protocol allows, and the disagreement only shows up at the size where
// nobody is looking.
func TestWriteLineAcceptsExactlyWhatReadLineAccepts(t *testing.T) {
	client, server := pipeConn(t)
	exact := bytes.Repeat([]byte("w"), MaxLineLen)
	errc := make(chan error, 1)
	go func() { errc <- client.writeLine(exact) }()

	line, err := server.ReadLine()
	if err != nil {
		t.Fatalf("a line of exactly MaxLineLen did not survive the round trip: %v", err)
	}
	if !bytes.Equal(line, exact) {
		t.Fatalf("got %d bytes, want %d intact", len(line), MaxLineLen)
	}
	if err := <-errc; err != nil {
		t.Fatalf("the writer refused a line the reader accepts: %v", err)
	}
}

// TestReadLineReportsEOFAfterTheLastFrame keeps the new reader's stream
// semantics: a clean close is io.EOF, once the frames before it are delivered.
func TestReadLineReportsEOFAfterTheLastFrame(t *testing.T) {
	a, b := net.Pipe()
	client, server := NewConn(a), NewConn(b)
	errc := peerErrs()
	go func() {
		writeRaw(errc, client, []byte("last\n"))
		_ = client.Close()
	}()
	if line, err := server.ReadLine(); err != nil || string(line) != "last" {
		t.Fatalf("last line: %q, %v", line, err)
	}
	if _, err := server.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF after a clean close, got %v", err)
	}
	noPeerError(t, errc)
	_ = server.Close()
}

// TestOversizedRequestIsAnsweredAndTheConnectionServesTheNext is the same
// contract through a real server over a real socket: the client's next
// request is answered on the same connection. This is the shape of the
// 🎯T661 incident — an unrelayable frame must not take the pending work with
// it.
func TestOversizedRequestIsAnsweredAndTheConnectionServesTheNext(t *testing.T) {
	path, _ := startTestServer(t)
	c := dialTest(t, path)

	errc := peerErrs()
	writeRaw(errc, c, append(bytes.Repeat([]byte("q"), 3*MaxLineLen), '\n'))
	noPeerError(t, errc)

	resp, err := c.ReadResponse()
	if err != nil {
		t.Fatalf("the server did not answer the oversized frame: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != CodeFrameTooLarge {
		t.Fatalf("got %+v, want a %s refusal", resp, CodeFrameTooLarge)
	}
	if !errors.Is(resp.Error.Err(), ErrFrameTooLarge) {
		t.Fatal("the refusal the server sent does not match ErrFrameTooLarge")
	}

	// The connection is still a working connection.
	got := roundTrip(t, c, &Request{ID: "after-1", Type: TypeStatus})
	if got.Type != TypeStatusResult || got.ID != "after-1" {
		t.Fatalf("the connection did not survive the oversized frame: %+v", got)
	}
}
