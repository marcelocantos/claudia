// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A turn that fails on its own while an interrupt is in flight.
func TestFleetAFailureDuringAnInterruptKeepsItsError(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	g := newGatedSeat(b)
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	r.startGated("b", g)
	first := r.send(FleetOwner, "b", "long job")
	waitFleet(t, "turn", func() bool { return r.working("b") })
	urgent := r.send(FleetOwner, "b", "urgent")
	go func() { _, _ = r.k.Expedite("f", FleetOwner, urgent, FleetExpediteInterrupt) }()
	<-g.entered
	b.death <- errors.New("plan exhausted: retry after reset") // seat stays alive
	time.Sleep(20 * time.Millisecond)
	g.release <- nil
	var seq uint64
	m := r.ownerMessage(&seq)
	if m.InReplyTo != first {
		t.Fatalf("unexpected %+v", m)
	}
	if !strings.Contains(m.Text, "plan exhausted") {
		t.Errorf("real failure lost; requester heard: %q", m.Text)
	}
}

// Seat with gated Send and gated Interrupt.
type bothGated struct {
	*fakeFleetSeat
	inSend, sendGate chan struct{}
	inInt            chan struct{}
	intGate          chan error
}

func (s *bothGated) Send(text string) error {
	s.inSend <- struct{}{}
	<-s.sendGate
	return s.fakeFleetSeat.Send(text)
}
func (s *bothGated) Interrupt() error { s.inInt <- struct{}{}; return <-s.intGate }

func TestFleetCloseDoesNotWaitOnAHungInterrupt(t *testing.T) {
	b := newFakeFleetSeat(fleetHold)
	s := &bothGated{fakeFleetSeat: b, inSend: make(chan struct{}, 4), sendGate: make(chan struct{}, 4), inInt: make(chan struct{}, 4), intGate: make(chan error)}
	r := newFleetRig(t, map[string]*fakeFleetSeat{})
	r.setStartFn(func(context.Context, *FleetSeatSpec) (FleetSeat, error) { return s, nil })
	r.start("b", FleetOwner)
	r.setStartFn(nil)
	r.send(FleetOwner, "b", "job")
	<-s.inSend
	urgent := r.send(FleetOwner, "b", "urgent")
	if _, err := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteInterrupt); err != nil {
		t.Fatal(err)
	}
	s.sendGate <- struct{}{}
	<-s.inInt // deferred interrupt now hangs
	done := make(chan struct{})
	go func() { r.k.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("Close blocked on a hung deferred interrupt")
		close(s.intGate)
		<-done
	}
}

// Send fails; an interrupt lands in the gap before the turn is judged.
type sendFails struct {
	*fakeFleetSeat
	inSend, sendGate chan struct{}
}

func (s *sendFails) Send(text string) error {
	s.inSend <- struct{}{}
	<-s.sendGate
	return errors.New("submit refused: busy")
}
func (s *sendFails) Interrupt() error { return nil } // idle seat: accepted

func TestFleetARefusedSendIsNotReportedAsInterrupted(t *testing.T) {
	masked := 0
	const runs = 50
	for i := 0; i < runs; i++ {
		func() {
			b := newFakeFleetSeat(fleetHold)
			s := &sendFails{fakeFleetSeat: b, inSend: make(chan struct{}, 4), sendGate: make(chan struct{}, 4)}
			r := newFleetRig(t, map[string]*fakeFleetSeat{})
			r.setStartFn(func(context.Context, *FleetSeatSpec) (FleetSeat, error) { return s, nil })
			r.start("b", FleetOwner)
			r.setStartFn(nil)
			first := r.send(FleetOwner, "b", "job")
			<-s.inSend
			urgent := r.send(FleetOwner, "b", "urgent")
			stop := make(chan struct{})
			go func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := r.k.Expedite("f", FleetOwner, urgent, FleetExpediteInterrupt); err != nil {
						return
					}
				}
			}()
			s.sendGate <- struct{}{}
			var seq uint64
			for {
				m := r.ownerMessage(&seq)
				if m.InReplyTo == first {
					if !strings.Contains(m.Text, "failed") {
						masked++
					}
					break
				}
			}
			close(stop)
			s.sendGate <- struct{}{}
			r.k.Close()
		}()
	}
	if masked > 0 {
		t.Errorf("send failure reported as an interrupted answer in %d/%d runs", masked, runs)
	}
}
