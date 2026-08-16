// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package grok

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// 🎯T31 — this package's tests used to bound every blocking receive
// with `waitFor = 5s`, so that "a hung expectation fails instead of stalling
// go test". `go test -timeout` already does that job, once per suite and with
// a full goroutine dump that names the goroutine actually stuck; the per-wait
// arm only ever named the helper. What the arm added was a second, silent
// assertion — that this machine schedules the fake's goroutines within five
// seconds — and under fleet load that assertion is the one that fails, turning
// somebody else's CPU contention into a RED for the client. The waits below
// are therefore plain receives: a slow machine makes them slower, never RED.

// fakeGrok is a hermetic stand-in for the xAI Realtime endpoint. It
// speaks real WebSocket over a loopback httptest server, so [Connect]
// exercises its production dial path with no network, credentials, or
// interface indirection in the client.
//
// Configure the fields before calling start; they are read-only once the
// server is running.
type fakeGrok struct {
	// upgradeStatus, when non-zero, refuses the WebSocket upgrade with
	// this HTTP status — the hermetic stand-in for an auth rejection at
	// the handshake.
	upgradeStatus int

	// firstReply produces the server's answer to the client's opening
	// session.update, which [Connect] blocks on. Nil sends
	// {"type":"session.updated"}; returning nil sends nothing, letting a
	// test hang up on the session instead.
	firstReply func(s *fakeSession, sessionUpdate map[string]any) any

	// url is the ws:// address to dial; set by start.
	url string

	// authHeaders carries the Authorization header of each handshake.
	authHeaders chan string

	// sessions carries each accepted connection.
	sessions chan *fakeSession
}

// fakeSession is one accepted connection: recv carries every decoded
// client message in order, readErr the error that ended the read loop.
type fakeSession struct {
	t       *testing.T
	conn    *websocket.Conn
	recv    chan map[string]any
	readErr chan error

	// sessionUpdate is the opening session.update, drained by
	// connectFake so recv holds only the messages a test provoked.
	sessionUpdate map[string]any
}

// start launches the server and registers cleanup. It returns the
// fakeGrok for chaining.
func (f *fakeGrok) start(t *testing.T) *fakeGrok {
	t.Helper()
	f.authHeaders = make(chan string, 8)
	f.sessions = make(chan *fakeSession, 8)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.authHeaders <- r.Header.Get("Authorization")
		if f.upgradeStatus != 0 {
			http.Error(w, "unauthorized", f.upgradeStatus)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("fakeGrok: accept: %v", err)
			return
		}
		s := &fakeSession{
			t:       t,
			conn:    conn,
			recv:    make(chan map[string]any, 64),
			readErr: make(chan error, 1),
		}
		f.sessions <- s

		ctx := context.Background()
		for first := true; ; first = false {
			_, data, err := conn.Read(ctx)
			if err != nil {
				s.readErr <- err
				close(s.recv)
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Errorf("fakeGrok: client sent invalid JSON %q: %v", data, err)
				return
			}
			s.recv <- msg
			if !first {
				continue
			}
			reply := any(map[string]any{"type": "session.updated"})
			if f.firstReply != nil {
				reply = f.firstReply(s, msg)
			}
			if reply != nil {
				s.send(reply)
			}
		}
	}))
	t.Cleanup(srv.Close)

	f.url = "ws://" + strings.TrimPrefix(srv.URL, "http://")
	return f
}

// dialArgs points a [Config] at this server.
func (f *fakeGrok) dialArgs() *DialArgs { return &DialArgs{URL: f.url} }

// session returns the connection accepted for the next [Connect].
func (f *fakeGrok) session(t *testing.T) *fakeSession {
	t.Helper()
	return <-f.sessions
}

// authHeader returns the Authorization header of the next handshake.
func (f *fakeGrok) authHeader(t *testing.T) string {
	t.Helper()
	return <-f.authHeaders
}

// send writes a server event to the client.
func (s *fakeSession) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.t.Errorf("fakeGrok: marshal server event: %v", err)
		return
	}
	// s.t.Context(): the write is to a loopback socket, so this bounds
	// cleanup only. A deadline here could abort a healthy write on a loaded
	// machine and report it as a fake-server error (🎯T31).
	if err := s.conn.Write(s.t.Context(), websocket.MessageText, data); err != nil {
		s.t.Errorf("fakeGrok: write server event: %v", err)
	}
}

// sendRaw writes bytes verbatim, for malformed-frame tests.
func (s *fakeSession) sendRaw(data []byte) {
	if err := s.conn.Write(s.t.Context(), websocket.MessageText, data); err != nil {
		s.t.Errorf("fakeGrok: write raw frame: %v", err)
	}
}

// next returns the next client message, failing the test on timeout.
func (s *fakeSession) next(t *testing.T) map[string]any {
	t.Helper()
	msg, ok := <-s.recv
	if !ok {
		t.Fatal("client connection closed while a message was expected")
	}
	return msg
}

// nextOfType skips client messages until one of the given type arrives.
func (s *fakeSession) nextOfType(t *testing.T, eventType string) map[string]any {
	t.Helper()
	// No deadline: next blocks on the channel, and a stream that ends without
	// the wanted type closes s.recv, which next reports as a real failure
	// (🎯T31). Only a client that keeps talking forever loops here, and
	// that is `go test -timeout`'s case.
	for {
		msg := s.next(t)
		if msg["type"] == eventType {
			return msg
		}
	}
}

// closeErr returns the error that ended the server's read loop — the
// hermetic view of how the client hung up.
func (s *fakeSession) closeErr(t *testing.T) error {
	t.Helper()
	return <-s.readErr
}

// connectFake dials the fake server and returns the connected client
// plus its server-side session, with the opening session.update already
// drained onto session.sessionUpdate. It fails the test if Connect errors.
func connectFake(t *testing.T, f *fakeGrok, cfg Config) (*Client, *fakeSession) {
	t.Helper()
	if cfg.APIKey == "" {
		cfg.APIKey = testAPIKey
	}
	cfg.Dial = f.dialArgs()

	// The connect context also drives the client's read loop, so it must
	// outlive Connect — cancel it at test cleanup, not on return.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	s := f.session(t)
	s.sessionUpdate = s.nextOfType(t, "session.update")
	return c, s
}

// testAPIKey is a syntactically plausible but entirely fictional key —
// nothing here authenticates against anything.
const testAPIKey = "test-key-not-a-secret"
