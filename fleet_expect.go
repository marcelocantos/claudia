// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A sense of time for senders (🎯T250). An idle agent hears nothing, so it
// cannot notice that a reply is late. A sender can say how soon it expects its
// message to be taken up and answered; if that passes, the kernel wakes it once
// with a notice from [FleetClockSender] saying where things stand. What to do
// then — wait, ask, expedite, give up — is the agent's judgment.
//
// Expediting moves a waiting message ahead. Steering folds it into the
// recipient's running turn, whose reply then also answers it; interrupting
// ends that turn, answering its requester that it was interrupted, so the
// message goes next. Both take authority over the recipient.

const (
	// FleetClockSender is the From of the kernel's notice that an expectation
	// passed.
	FleetClockSender = "clock"
	// Expedite modes.
	FleetExpediteSteer     = "steer"
	FleetExpediteInterrupt = "interrupt"

	fleetExpectAccept = "accept"
	fleetExpectAnswer = "answer"

	fleetSteeredNote = "[steered into your running turn: your reply at the end of this turn also answers it]"
)

// errFleetNotWaiting means a message is in no queue: taken up, answered, or
// never sent.
var errFleetNotWaiting = errors.New("is not waiting in any queue")

type fleetExpectState struct {
	From     string     `json:"from"`
	To       string     `json:"to"`
	Kind     string     `json:"kind"`
	SentAt   time.Time  `json:"sent_at"`
	AcceptBy *time.Time `json:"accept_by,omitempty"`
	AnswerBy *time.Time `json:"answer_by,omitempty"`
	Accepted *time.Time `json:"accepted,omitempty"`
}

// expectLocked records a sender's expectations of a message and arms them.
// k.mu held.
func (k *FleetKernel) expectLocked(fleet string, group *fleetGroupState, m *FleetMessage, accept, answer time.Duration) {
	if accept <= 0 && answer <= 0 {
		return
	}
	now := k.clock.Now()
	e := &fleetExpectState{From: m.From, To: m.To, Kind: m.Kind, SentAt: now}
	if accept > 0 {
		at := now.Add(accept)
		e.AcceptBy = &at
	}
	if answer > 0 {
		at := now.Add(answer)
		e.AnswerBy = &at
	}
	if group.Expects == nil {
		group.Expects = map[string]*fleetExpectState{}
	}
	group.Expects[m.ID] = e
	k.armExpectsLocked(fleet, m.ID, e)
}

func (k *FleetKernel) armExpectsLocked(fleet, id string, e *fleetExpectState) {
	if e.AcceptBy != nil {
		k.armExpectLocked(fleet, id, fleetExpectAccept, e, *e.AcceptBy)
	}
	if e.AnswerBy != nil {
		k.armExpectLocked(fleet, id, fleetExpectAnswer, e, *e.AnswerBy)
	}
}

func fleetExpectKey(fleet, id, which string) string { return fleetKey(fleet, id+"/"+which) }

// armExpectLocked waits on the kernel's clock. One already due, as after a
// restart, fires at once. k.mu held, kernel open.
func (k *FleetKernel) armExpectLocked(fleet, id, which string, e *fleetExpectState, at time.Time) {
	key := fleetExpectKey(fleet, id, which)
	stop := make(chan struct{})
	k.timers[key] = stop
	fired := k.clock.After(max(at.Sub(k.clock.Now()), 0))
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		select {
		case <-fired:
		case <-stop:
			return
		case <-k.ctx.Done():
			return
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		if k.closed || k.timers[key] != stop {
			return
		}
		delete(k.timers, key)
		group := k.state.Fleets[fleet]
		if group == nil || group.Expects[id] != e {
			return
		}
		k.expectMissedLocked(fleet, group, id, e, which)
		if err := k.saveLocked(); err != nil {
			k.logf("fleet: save after expectation", "fleet", fleet, "err", err)
		}
	}()
}

func (k *FleetKernel) disarmExpectLocked(fleet, id, which string) {
	key := fleetExpectKey(fleet, id, which)
	if stop := k.timers[key]; stop != nil {
		close(stop)
		delete(k.timers, key)
	}
}

// acceptedLocked notes that a message was taken up, by a turn or a steer.
// k.mu held.
func (k *FleetKernel) acceptedLocked(fleet string, group *fleetGroupState, id string) {
	e := group.Expects[id]
	if e == nil || e.Accepted != nil {
		return
	}
	now := k.clock.Now()
	e.Accepted = &now
	e.AcceptBy = nil
	k.disarmExpectLocked(fleet, id, fleetExpectAccept)
	if e.AnswerBy == nil {
		delete(group.Expects, id)
	}
}

// answeredLocked ends every expectation of a message. k.mu held.
func (k *FleetKernel) answeredLocked(fleet string, group *fleetGroupState, id string) {
	if group.Expects[id] == nil {
		return
	}
	delete(group.Expects, id)
	k.disarmExpectLocked(fleet, id, fleetExpectAccept)
	k.disarmExpectLocked(fleet, id, fleetExpectAnswer)
}

// dropExpectsOfLocked forgets what a departing agent was waiting for, so a
// successor taking its name is not woken for it. k.mu held.
func (k *FleetKernel) dropExpectsOfLocked(fleet string, group *fleetGroupState, name string) {
	for id, e := range group.Expects {
		if e.From == name {
			k.answeredLocked(fleet, group, id)
		}
	}
}

// expectMissedLocked tells the sender, once, that an expectation passed and
// where its message stands. k.mu held.
func (k *FleetKernel) expectMissedLocked(fleet string, group *fleetGroupState, id string, e *fleetExpectState, which string) {
	now := k.clock.Now()
	age := fleetDuration(now.Sub(e.SentAt))
	var text string
	if which == fleetExpectAccept {
		e.AcceptBy = nil
		text = fmt.Sprintf("Your %s %s to %s has not been taken up %s after you sent it, which is longer than you expected.", e.Kind, id, e.To, age)
	} else {
		e.AnswerBy = nil
		text = fmt.Sprintf("Your request %s to %s has no answer %s after you sent it, which is longer than you expected.", id, e.To, age)
	}
	if where := k.whereLocked(fleet, group, id, e); where != "" {
		text += " " + where
	}
	text += " You can keep waiting, ask, expedite it if you have authority over " + e.To + ", or change course."
	if e.AcceptBy == nil && e.AnswerBy == nil {
		delete(group.Expects, id)
	}
	k.deliverLocked(fleet, &FleetMessage{Kind: FleetNotice, From: FleetClockSender, To: e.From, Text: text})
}

// whereLocked says where a message stands with its recipient. k.mu held.
func (k *FleetKernel) whereLocked(fleet string, group *fleetGroupState, id string, e *fleetExpectState) string {
	a := group.Agents[e.To]
	if a == nil {
		return e.To + " is gone."
	}
	now := k.clock.Now()
	turn := ""
	if a.Current != nil {
		if a.Current.ID == id {
			return fmt.Sprintf("%s took it up %s ago and is still working on it.", a.Name, fleetDuration(now.Sub(a.CurrentSince)))
		}
		turn = fmt.Sprintf("its current turn, on %s from %s, has run %s", a.Current.ID, a.Current.From, fleetDuration(now.Sub(a.CurrentSince)))
	}
	for _, m := range a.Steered {
		if m.ID == id {
			return fmt.Sprintf("It was steered into %s's turn, which has run %s and is still going.", a.Name, fleetDuration(now.Sub(a.CurrentSince)))
		}
	}
	for i, m := range a.Inbox {
		if m.ID != id {
			continue
		}
		where := fmt.Sprintf("It is number %d in %s's queue", i+1, a.Name)
		switch {
		case turn != "":
			where += ", and " + turn
		case k.running[fleetKey(fleet, a.Name)] == nil:
			where += ", and " + a.Name + " is not running yet"
		case k.blockedLocked(fleet, group, a):
			where += ", and a budget over " + a.Name + " is spent, so it takes no turns"
		}
		return where + "."
	}
	if e.Accepted != nil {
		return fmt.Sprintf("%s took it up %s ago.", a.Name, fleetDuration(now.Sub(*e.Accepted)))
	}
	return ""
}

// queuedLocked finds a message waiting in a mailbox. k.mu held.
func queuedLocked(group *fleetGroupState, id string) (*fleetAgentState, int) {
	for _, a := range group.Agents {
		for i, m := range a.Inbox {
			if m.ID == id {
				return a, i
			}
		}
	}
	return nil, -1
}

// Expedite moves a waiting message ahead of the rest. [FleetExpediteSteer]
// folds it into its recipient's running turn; [FleetExpediteInterrupt] ends
// that turn so the message goes next. Either way it goes first in the queue
// if it cannot be done. It takes authority over the recipient, and returns
// what happened.
func (k *FleetKernel) Expedite(fleet, caller, id, mode string) (string, error) {
	if mode != FleetExpediteSteer && mode != FleetExpediteInterrupt {
		return "", fmt.Errorf("expedite by %q or %q, not %q", FleetExpediteSteer, FleetExpediteInterrupt, mode)
	}
	k.mu.Lock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		k.mu.Unlock()
		return "", err
	}
	a, i := queuedLocked(group, id)
	if a == nil {
		k.mu.Unlock()
		return "", fmt.Errorf("message %q %w: it may have been taken up or answered already", id, errFleetNotWaiting)
	}
	if !authorized(group, caller, a.Name) {
		k.mu.Unlock()
		return "", fmt.Errorf("agent %q has no authority over %q", caller, a.Name)
	}
	m := a.Inbox[i]
	a.Inbox = slices.Insert(slices.Delete(a.Inbox, i, i+1), 0, m)
	rt := k.running[fleetKey(fleet, a.Name)]
	if a.Current == nil || rt == nil {
		k.saveOrLogLocked(fleet, "expedite")
		k.mu.Unlock()
		return fmt.Sprintf("%s is between turns, so %s goes next", a.Name, id), nil
	}
	turn := rt.turns

	if mode == FleetExpediteInterrupt {
		if !rt.live {
			rt.pending = &fleetPendingInterrupt{by: caller, forID: id}
			k.saveOrLogLocked(fleet, "expedite")
			k.mu.Unlock()
			return fmt.Sprintf("%s's turn is just starting; it will be interrupted once it has, and %s goes next", a.Name, id), nil
		}
		rt.interrupting++
		k.saveOrLogLocked(fleet, "expedite")
		k.mu.Unlock()
		if err := k.interruptSeat(rt, turn, caller, id); err != nil {
			return fmt.Sprintf("could not interrupt %s (%v), so %s goes next when its turn ends", a.Name, err, id), nil
		}
		return fmt.Sprintf("asked %s to stop its turn; %s goes next", a.Name, id), nil
	}

	// A steered message is a turn's worth of work, so a spent budget holds it
	// as it would hold a turn.
	if k.blockedLocked(fleet, group, a) {
		k.saveOrLogLocked(fleet, "expedite")
		k.mu.Unlock()
		return fmt.Sprintf("a budget over %s is spent, so %s waits first in its queue", a.Name, id), nil
	}
	// Steered messages are saved as part of the turn, so a restart delivers
	// them again; steering marks one until the seat has taken it.
	a.Inbox = a.Inbox[1:]
	m.steering = true
	a.Steered = append(a.Steered, m)
	text := fleetSteeredNote + "\n" + fleetStamp(k.clock.Now(), m, time.Time{}) + "\n" + formatFleetMessage(m)
	k.saveOrLogLocked(fleet, "steer")
	k.mu.Unlock()

	err = rt.seat.Steer(text)

	k.mu.Lock()
	defer k.mu.Unlock()
	m.steering = false
	if k.closed {
		// The saved state holds it as steered, so the next kernel delivers it.
		return fmt.Sprintf("Claudia is restarting; %s will reach %s", id, a.Name), nil
	}
	at := slices.Index(a.Steered, m)
	if at < 0 {
		// Retired or redelivered meanwhile: whatever took it answers it.
		return fmt.Sprintf("%s went to %s", id, a.Name), nil
	}
	if err == nil && rt.turns == turn && a.Current != nil && k.running[fleetKey(fleet, a.Name)] == rt {
		k.acceptedLocked(fleet, group, id)
		if !m.Charged {
			k.chargeTurnLocked(group, a)
			m.Charged = true
		}
		k.saveOrLogLocked(fleet, "steer")
		return fmt.Sprintf("steered %s into %s's running turn; its reply answers it", id, a.Name), nil
	}
	// The turn ended first, or the seat cannot steer: it goes next instead. A
	// steer the seat did take after its turn ended reaches it twice, which is
	// better than not at all. A stopping agent still answers its queue as it
	// retires.
	a.Steered = slices.Delete(a.Steered, at, at+1)
	a.Inbox = append([]*FleetMessage{m}, a.Inbox...)
	if live := k.running[fleetKey(fleet, a.Name)]; live != nil {
		live.wake.Signal()
	}
	k.saveOrLogLocked(fleet, "steer")
	if err != nil {
		return fmt.Sprintf("could not steer into %s's turn (%v), so %s goes next", a.Name, err, id), nil
	}
	return fmt.Sprintf("%s's turn ended first, so %s goes next", a.Name, id), nil
}

// interruptSeat interrupts turn, for which the caller has counted an
// interrupt in progress. The seat cannot be told which turn to interrupt, so
// the run loop starts no other turn, and judges this one, until it returns.
func (k *FleetKernel) interruptSeat(rt *fleetRuntime, turn int, by, forID string) error {
	err := rt.seat.Interrupt()
	k.mu.Lock()
	defer k.mu.Unlock()
	rt.interrupting--
	rt.wake.Broadcast()
	if err == nil && rt.turns == turn {
		rt.interruptTurn, rt.interruptedBy, rt.interruptFor = turn, by, forID
	}
	return err
}

func (k *FleetKernel) saveOrLogLocked(fleet, what string) {
	if err := k.saveLocked(); err != nil {
		k.logf("fleet: save after "+what, "fleet", fleet, "err", err)
	}
}

// Deliver sends like [FleetKernel.Send] and then expedites as args.Mode asks,
// returning the message ID and what expediting did.
func (k *FleetKernel) Deliver(fleet string, args *FleetSendArgs) (id, note string, err error) {
	id, err = k.send(fleet, args)
	if err != nil || args.Mode == "" {
		return id, "", err
	}
	note, err = k.Expedite(fleet, args.From, id, args.Mode)
	if errors.Is(err, errFleetNotWaiting) {
		return id, args.To + " took it up straight away", nil
	}
	if err != nil {
		return id, "not expedited: " + err.Error(), nil
	}
	return id, note, nil
}

// turnMessages takes the current turn's messages, to deliver them again.
func (a *fleetAgentState) turnMessages() []*FleetMessage {
	var out []*FleetMessage
	if a.Current != nil {
		out = append(out, a.Current)
	}
	out = append(out, a.Steered...)
	for _, m := range out {
		m.Redelivered = true
	}
	return out
}

// takeSteered takes the messages steered into the turn that just ended,
// leaving any still on their way in.
func (a *fleetAgentState) takeSteered() []*FleetMessage {
	var done, pending []*FleetMessage
	for _, m := range a.Steered {
		if m.steering {
			pending = append(pending, m)
		} else {
			done = append(done, m)
		}
	}
	a.Steered = pending
	return done
}

// fleetInterruptedReply marks a reply from a turn an interrupt reached.
func fleetInterruptedReply(reply, by, forID string) string {
	note := fmt.Sprintf("[%s interrupted this turn to take up %s first, so this answer may be incomplete.]", by, forID)
	if reply == "" || strings.HasSuffix(reply, FleetNoReply) {
		return note + " It had not replied."
	}
	return reply + "\n\n" + note
}
