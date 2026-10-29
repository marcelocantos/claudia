// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
	"testing"
)

// Production grant-by-name release must interrupt before it removes the
// connection; closing a handle alone does not stop a sidecar model stream.
func TestT238ReleaseStopsInFlightTurnBeforeRegrant(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	c := rawSeat(t, f.sock, "release-race")
	sent := rawCall(t, c, &broker.Request{ID: "send", Type: broker.TypeSend,
		Send: &broker.SendRequest{Name: "release-race", Text: "delayed old stream"}})
	if sent.Type != broker.TypeSent {
		t.Fatalf("send: %+v", sent)
	}
	old := f.seat(0)
	if !old.inFlight.Load() {
		t.Fatal("fixture did not start old turn")
	}
	released := rawCall(t, c, &broker.Request{ID: "release", Type: broker.TypeRelease,
		Release: &broker.ReleaseRequest{Name: "release-race", Disposition: broker.DispositionStop}})
	if released.Type != broker.TypeReleased {
		t.Fatalf("release: %+v", released)
	}
	interrupts, _, stops := old.counts()
	if interrupts != 1 || stops != 1 || old.inFlight.Load() {
		t.Fatalf("released live seat: interrupts=%d stops=%d inFlight=%t", interrupts, stops, old.inFlight.Load())
	}
	// The backend has no old work to bill when the next grant starts.
	c2 := rawSeat(t, f.sock, "release-race")
	_ = c2
	if f.seatCount() != 2 || old.inFlight.Load() {
		t.Fatal("old provider survived next grant")
	}
}

// A second consumer cannot win the name while the first Release waits for
// the old stream to settle. The old grant is fenced across the unlocked wait.
func TestT238ReleaseFencesConcurrentGrant(t *testing.T) {
	f := newFixture(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		s := newSeat()
		f.mu.Lock()
		f.seats = append(f.seats, s)
		f.mu.Unlock()
		f.ensureStubTranscript(cfg)
		ops := s.ops()
		ops.Interrupt = func() error {
			close(entered)
			<-resume
			s.mu.Lock()
			s.interrupts++
			s.mu.Unlock()
			s.inFlight.Store(false)
			return nil
		}
		return claudia.StartStub(ctx, cfg, ops)
	}
	f.boot(t, nil)
	owner := rawSeat(t, f.sock, "fenced")
	sent := rawCall(t, owner, &broker.Request{ID: "send", Type: broker.TypeSend,
		Send: &broker.SendRequest{Name: "fenced", Text: "old stream"}})
	if sent.Type != broker.TypeSent {
		t.Fatalf("send: %+v", sent)
	}
	finished := make(chan *broker.Response, 1)
	go func() {
		finished <- rawCall(t, owner, &broker.Request{ID: "stop", Type: broker.TypeRelease,
			Release: &broker.ReleaseRequest{Name: "fenced", Disposition: broker.DispositionStop}})
	}()
	<-entered
	contender, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: "fenced", WorkDir: t.TempDir(), SessionID: "sid-fenced", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	rejected := rawCall(t, contender, &broker.Request{ID: "contender", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "fenced", Def: def}})
	close(resume)
	if rejected.Type != broker.TypeError || rejected.Error.Code != broker.CodeGrantHeld {
		t.Fatalf("concurrent grant = %+v, want grant_held", rejected)
	}
	if done := <-finished; done.Type != broker.TypeReleased {
		t.Fatalf("release = %+v", done)
	}
}

// Reclaim cannot silently rewrite an explicit provider request to the
// broker's old live provider. A caller needs a typed refusal to choose
// whether to switch or reattach to the provider actually running.
func TestT238AdoptRefusesStaleProviderInsteadOfSuccessfulOldBackend(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	old := rawSeat(t, f.sock, "moved")
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "old owner detached", func() bool {
		f.d.mu.Lock()
		defer f.d.mu.Unlock()
		g := f.d.grants["moved"]
		return g != nil && g.owner == nil
	})
	contender, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: "moved", Provider: claudia.ProviderGrok, WorkDir: t.TempDir(), SessionID: "sid-moved", TermLogPath: "-",
	}})
	if err != nil {
		t.Fatal(err)
	}
	refused := rawCall(t, contender, &broker.Request{ID: "new-provider", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: "moved", Def: def, Adopt: true}})
	if refused.Type != broker.TypeError || refused.Error.Code != broker.CodeProviderMismatch || refused.Error.Field != "provider" {
		t.Fatalf("adopt returned %+v instead of provider_mismatch", refused.Error)
	}
	if f.seatCount() != 1 || f.d.reg.Def("moved").Provider == claudia.ProviderGrok {
		t.Fatalf("mismatch changed live provider or started another seat: count=%d def=%+v", f.seatCount(), f.d.reg.Def("moved"))
	}
}
