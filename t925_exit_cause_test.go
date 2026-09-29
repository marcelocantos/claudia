// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// t925Seat is a broker-backed handle whose events drain from a pipe the
// test plays the broker on.
func t925Seat(t *testing.T) (*brokerAgentBackend, *Agent, *broker.Conn, *brokerClient) {
	t.Helper()
	b, peer := clientOverPipe(t)
	a := &Agent{alive: true}
	backend := &brokerAgentBackend{client: b, agent: a, ready: make(chan struct{}), subscribed: make(chan struct{})}
	backend.queue.init()
	b.setPush(backend.onPush)
	close(backend.ready)
	close(backend.subscribed)
	go backend.drain()
	return backend, a, broker.NewConn(peer), b
}

func t925WaitDead(t *testing.T, a *Agent) {
	t.Helper()
	select {
	case <-a.deadSignal():
	// 🎯T97 exemption: a failsafe on the death the test already waits for.
	case <-time.After(5 * time.Second):
		t.Fatal("handle never died")
	}
}

// 🎯T925: a broker-backed handle knows why it died. The broker ending the
// connection (it stopped or restarted) is ExitCauseBrokerLost; the broker
// reporting the seat gone carries its reason; this client's own Close is a
// deliberate stop and carries none.
func TestT925BrokerHandleRecordsWhyItDied(t *testing.T) {
	t.Run("broker ended the connection", func(t *testing.T) {
		_, a, peer, _ := t925Seat(t)
		if a.ExitCause() != "" {
			t.Fatalf("a live handle has an exit cause %q", a.ExitCause())
		}
		_ = peer.Close()
		t925WaitDead(t, a)
		if got := a.ExitCause(); got != ExitCauseBrokerLost {
			t.Fatalf("exit cause %q, want %q", got, ExitCauseBrokerLost)
		}
	})
	t.Run("broker reported the seat gone", func(t *testing.T) {
		_, a, peer, _ := t925Seat(t)
		if err := peer.WriteResponse(&broker.Response{Type: broker.TypeAgentGone,
			AgentGone: &broker.AgentGoneMessage{Name: "s", Reason: "process exited"}}); err != nil {
			t.Fatal(err)
		}
		t925WaitDead(t, a)
		if got := a.ExitCause(); got != "claudia broker: seat gone: process exited" {
			t.Fatalf("exit cause %q", got)
		}
	})
	t.Run("broker said it was stopping on purpose", func(t *testing.T) {
		_, a, peer, _ := t925Seat(t)
		if err := peer.WriteResponse(&broker.Response{Type: broker.TypeEvent,
			Event: &broker.EventMessage{Kind: broker.EventShutdown, Detail: broker.ShutdownPlanned}}); err != nil {
			t.Fatal(err)
		}
		_ = peer.Close()
		t925WaitDead(t, a)
		if got := a.ExitCause(); got != ExitCauseBrokerRestarted {
			t.Fatalf("exit cause %q, want %q (jevons T944)", got, ExitCauseBrokerRestarted)
		}
	})
	t.Run("this client closed it", func(t *testing.T) {
		_, a, _, client := t925Seat(t)
		client.Close()
		t925WaitDead(t, a)
		if got := a.ExitCause(); got != "" {
			t.Fatalf("a deliberate close carries exit cause %q", got)
		}
	})
}
