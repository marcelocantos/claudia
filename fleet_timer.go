// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Time reaches an agent only as a message (🎯T248.1). The kernel keeps one
// durable timer table on one clock with one firing rule, and stamps every
// delivery with the time, so an agent can judge how long things take without a
// clock of its own. How long to wait, when to give up and what is stale stay
// with the agent.

// fleetMinEvery is the shortest repeat, because each firing is a turn.
const fleetMinEvery = time.Minute

// FleetSendArgs is one message, now or scheduled.
type FleetSendArgs struct {
	From string
	To   string
	// Kind is [FleetRequest], the default, or [FleetNotice] from the owner.
	Kind string
	Text string
	// After or At schedules the first delivery. Neither means now, unless Every
	// is set, which then also sets the first delay.
	After time.Duration
	At    time.Time
	// Every repeats the message until it is cancelled or its target goes. Only
	// the owner, or an agent scheduling itself, may repeat.
	Every time.Duration
}

// FleetTimerStatus is one scheduled message.
type FleetTimerStatus struct {
	ID           string    `json:"id"`
	From         string    `json:"from"`
	To           string    `json:"to"`
	Kind         string    `json:"kind"`
	Due          time.Time `json:"due"`
	EverySeconds float64   `json:"every_seconds,omitempty"`
	Text         string    `json:"text"`
}

type fleetTimerState struct {
	At      time.Time     `json:"at"`
	Every   time.Duration `json:"every,omitempty"`
	Fires   int           `json:"fires,omitempty"`
	Message *FleetMessage `json:"message"`
}

// Send delivers a message now, or schedules it, and returns the message ID or,
// when scheduled, the timer ID that [FleetKernel.Cancel] takes. A message to
// yourself must be scheduled: it is a timer, and needs no response.
func (k *FleetKernel) Send(fleet string, args *FleetSendArgs) (string, error) {
	if args == nil {
		return "", errors.New("nothing to send")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		return "", err
	}
	from, to, kind := args.From, args.To, args.Kind
	if to != FleetOwner && group.Agents[to] == nil {
		return "", fmt.Errorf("no agent named %q", to)
	}
	if kind == "" {
		kind = FleetRequest
	}
	if kind != FleetRequest && kind != FleetNotice {
		return "", fmt.Errorf("cannot send a message of kind %q", kind)
	}
	if args.After < 0 || args.Every < 0 {
		return "", errors.New("a delay cannot be negative")
	}
	if args.After > 0 && !args.At.IsZero() {
		return "", errors.New("give a delay or a time, not both")
	}
	if args.Every > 0 && args.Every < fleetMinEvery {
		return "", fmt.Errorf("a repeat must be at least %s apart", fleetMinEvery)
	}
	if args.Every > 0 && from != to && from != FleetOwner {
		return "", errors.New("an agent may repeat a message only to itself")
	}
	scheduled := args.After > 0 || !args.At.IsZero() || args.Every > 0
	if from == to && !scheduled {
		return "", errors.New("a message to yourself needs a delay")
	}
	if group.Sent >= group.MaxMessages {
		return "", fmt.Errorf("message limit %d reached", group.MaxMessages)
	}
	if from == to {
		kind = FleetTimer
	}
	// A repeat counts once against the message limit; the budget limits what its
	// turns spend.
	group.Sent++
	m := &FleetMessage{Kind: kind, From: from, To: to, Text: boundFleetText(args.Text)}
	if !scheduled {
		m.ID = k.nextIDLocked(group)
		k.deliverLocked(fleet, m)
	} else {
		now := k.clock.Now()
		due := args.At
		switch {
		case args.After > 0:
			due = now.Add(args.After)
		case due.IsZero():
			due = now.Add(args.Every)
		}
		// A repeat starting in the past starts now: catching up on firings from
		// before it existed would be meaningless.
		if args.Every > 0 && due.Before(now) {
			due = now
		}
		m.ID = k.nextTimerIDLocked(group)
		timer := &fleetTimerState{At: due, Every: args.Every, Message: m}
		if group.Timers == nil {
			group.Timers = map[string]*fleetTimerState{}
		}
		group.Timers[m.ID] = timer
		k.armTimerLocked(fleet, m.ID, timer)
	}
	if err := k.saveLocked(); err != nil {
		return "", err
	}
	return m.ID, nil
}

func (k *FleetKernel) nextTimerIDLocked(group *fleetGroupState) string {
	group.NextID++
	return fmt.Sprintf("t%d", group.NextID)
}

// Cancel removes a scheduled message. The owner can cancel any; an agent, only
// its own.
func (k *FleetKernel) Cancel(fleet, caller, id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	group, err := k.groupLocked(fleet)
	if err != nil {
		return err
	}
	timer := group.Timers[id]
	if timer == nil {
		return fmt.Errorf("no scheduled message %q", id)
	}
	if caller != FleetOwner && timer.Message.From != caller {
		return fmt.Errorf("agent %q may cancel only its own scheduled messages", caller)
	}
	k.dropTimerLocked(fleet, group, id)
	k.eventLocked(fleet, &FleetEvent{Kind: FleetEventCancel, Agent: caller, Text: id})
	return k.saveLocked()
}

func (k *FleetKernel) dropTimerLocked(fleet string, group *fleetGroupState, id string) {
	delete(group.Timers, id)
	key := fleetKey(fleet, id)
	if stop := k.timers[key]; stop != nil {
		close(stop)
		delete(k.timers, key)
	}
}

// dropTimersOfLocked removes everything a departing agent scheduled and
// everything scheduled to it, so that a successor taking its name inherits
// none of it. A scheduled request to it is answered now with the failure it
// would have met when due. k.mu held.
func (k *FleetKernel) dropTimersOfLocked(fleet string, group *fleetGroupState, name, failure string) {
	for id, t := range group.Timers {
		m := t.Message
		if m.From != name && m.To != name {
			continue
		}
		k.dropTimerLocked(fleet, group, id)
		if m.To == name && m.From != name && m.Kind == FleetRequest && t.Every <= 0 {
			k.deliverLocked(fleet, &FleetMessage{Kind: FleetResponse, From: name, To: m.From, InReplyTo: id, Text: failure})
		}
	}
}

// armTimerLocked waits for a timer on the kernel's clock. A timer already due,
// as after a restart, fires at once. k.mu held, kernel open.
func (k *FleetKernel) armTimerLocked(fleet, id string, timer *fleetTimerState) {
	key := fleetKey(fleet, id)
	stop := make(chan struct{})
	k.timers[key] = stop
	wait := max(timer.At.Sub(k.clock.Now()), 0)
	fired := k.clock.After(wait)
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
		if group == nil || group.Timers[id] != timer {
			return
		}
		k.fireTimerLocked(fleet, group, id, timer)
		if err := k.saveLocked(); err != nil {
			k.logf("fleet: save after timer", "fleet", fleet, "err", err)
		}
	}()
}

// fireTimerLocked delivers one firing. A repeat that missed firings, while the
// broker was down or while its target could not take them, fires once, saying
// how many it missed, and keeps its own schedule rather than restarting from now.
// At most one firing of a repeat waits in a mailbox: a later one adds to that
// one's missed count. k.mu held.
func (k *FleetKernel) fireTimerLocked(fleet string, group *fleetGroupState, id string, timer *fleetTimerState) {
	now := k.clock.Now()
	m := *timer.Message
	due := timer.At
	m.Due = &due
	if timer.Every <= 0 {
		delete(group.Timers, id)
		k.deliverLocked(fleet, &m)
		return
	}
	target := group.Agents[m.To]
	if m.To != FleetOwner && target == nil {
		delete(group.Timers, id)
		return
	}
	// Computed from the remainder, not by multiplying, so that a due time far in
	// the past cannot overflow and move the schedule backwards.
	missed := 0
	if late := now.Sub(timer.At); late >= timer.Every {
		missed = int(late / timer.Every)
		timer.At = now.Add(timer.Every - late%timer.Every)
	} else {
		timer.At = timer.At.Add(timer.Every)
	}
	k.armTimerLocked(fleet, id, timer)
	if target != nil {
		for _, waiting := range target.Inbox {
			if waiting.Timer == id {
				waiting.Missed += missed + 1
				return
			}
		}
	}
	timer.Fires++
	m.Missed = missed
	m.ID = fmt.Sprintf("%s.%d", id, timer.Fires)
	m.Timer = id
	k.deliverLocked(fleet, &m)
}

func (k *FleetKernel) timerStatusLocked(group *fleetGroupState) []FleetTimerStatus {
	out := make([]FleetTimerStatus, 0, len(group.Timers))
	for id, t := range group.Timers {
		text := t.Message.Text
		if len(text) > fleetStatusText {
			text = text[:fleetStatusText] + "…"
		}
		out = append(out, FleetTimerStatus{ID: id, From: t.Message.From, To: t.Message.To, Kind: t.Message.Kind,
			Due: t.At, EverySeconds: t.Every.Seconds(), Text: text})
	}
	return out
}

// fleetStamp is the first line of every delivery: the time now, and what time
// has done to this message. Relative values, because models are poor at
// arithmetic on timestamps and do not know today's date.
func fleetStamp(now time.Time, m *FleetMessage, lastTurn time.Time) string {
	parts := []string{"now " + now.Local().Format("Mon 2 Jan 2006 15:04 MST")}
	if !m.SentAt.IsZero() {
		if age := now.Sub(m.SentAt); age >= time.Second {
			parts = append(parts, "sent "+fleetDuration(age)+" ago")
		}
	}
	if m.Due != nil {
		timer := m.ID
		if m.Timer != "" {
			timer = m.Timer
		}
		note := "scheduled " + timer + " for " + m.Due.Local().Format("15:04")
		if late := m.SentAt.Sub(*m.Due); late >= time.Minute {
			note += ", delivered " + fleetDuration(late) + " late"
		}
		if m.Missed > 0 {
			note += fmt.Sprintf(", %d earlier firings missed", m.Missed)
		}
		parts = append(parts, note)
	}
	if !lastTurn.IsZero() {
		parts = append(parts, "your last turn ended "+fleetDuration(now.Sub(lastTurn))+" ago")
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// fleetDuration is a short human duration: 45s, 12m, 3h12m, 2d4h.
func fleetDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
