// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

//claudia:policy

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Seat lifecycle on the Registry (🎯T75.2, 🎯T75.6). Bringing seats back
// after a restart, telling them what happened, and noticing that a seat's
// process died are things any application that keeps agents running needs,
// with or without a daemon. The daemon uses exactly this and forwards the
// events onto its tail.

// SeatEventKind names one seat lifecycle event.
type SeatEventKind string

const (
	// SeatResumed: [Registry.ResumeAll] brought the seat back. How says
	// whether it was adopted, launched or reminted.
	SeatResumed SeatEventKind = "resumed"
	// SeatResumeFailed: the seat could not be brought back. Err says why.
	SeatResumeFailed SeatEventKind = "resume_failed"
	// SeatNudged: a relaunched seat was sent the restart nudge.
	SeatNudged SeatEventKind = "nudged"
	// SeatGone: a launched seat's process is no longer alive.
	SeatGone SeatEventKind = "gone"
)

// ResumeHow says how [Registry.ResumeAll] brought a seat back.
type ResumeHow string

const (
	// ResumeAdopted: the seat's process was still running and was reused.
	ResumeAdopted ResumeHow = "adopted"
	// ResumeLaunched: the process was gone; it was started again on its
	// saved conversation.
	ResumeLaunched ResumeHow = "launched"
	// ResumeReminted: the saved conversation could not be resumed, so the
	// seat was started on a fresh session.
	ResumeReminted ResumeHow = "reminted"
)

// SeatEvent is one seat lifecycle event.
type SeatEvent struct {
	Kind SeatEventKind
	// Name is the seat's registered name.
	Name string
	// SessionID is the seat's session when the event was published.
	SessionID string
	// How is set on SeatResumed.
	How ResumeHow
	// Err is set on SeatResumeFailed.
	Err error
	// Agent is the live handle on SeatResumed, SeatNudged and SeatGone.
	Agent *Agent
	At    time.Time
}

// DefaultRestartNudge is what a relaunched seat is told by
// [Registry.ResumeAll]; %s is the restart time.
const DefaultRestartNudge = "[claudia] The host restarted at %s. This session was resumed from its saved transcript; " +
	"any tool call, build or process that was running before the restart did not finish. " +
	"Review where you were and continue the task you were working on. If you were waiting on " +
	"something (a build, a test, another agent), check its state again before assuming it completed."

const (
	// NoRestartNudge as [ResumeArgs.Nudge] sends nothing.
	NoRestartNudge = "-"
	// defaultResumeConcurrency bounds provider starts during a resume: a
	// host coming back with dozens of seats must not start them all at once.
	defaultResumeConcurrency = 2
)

// DefaultSeatWatchInterval is how often a Registry with seat-event
// subscribers probes its seats' liveness.
const DefaultSeatWatchInterval = 5 * time.Second

// seatClock is the Registry's clock, read under seatMu because SetClock may
// replace it.
func (r *Registry) seatClock() Clock {
	r.seatMu.Lock()
	defer r.seatMu.Unlock()
	return r.clock
}

// SubscribeSeatEvents registers fn for this Registry's seat lifecycle
// events and returns an id for [Registry.UnsubscribeSeatEvents]. fn runs on
// the publishing goroutine, in order: a SeatResumed subscriber returns
// before the seat is nudged, so it can subscribe to the Agent first and
// miss nothing of the turn the nudge starts.
//
// Liveness is watched while at least one subscriber is registered; a seat
// whose process dies produces one SeatGone.
func (r *Registry) SubscribeSeatEvents(fn func(SeatEvent)) int64 {
	r.seatMu.Lock()
	defer r.seatMu.Unlock()
	r.seatNextID++
	id := r.seatNextID
	if r.seatSubs == nil {
		r.seatSubs = make(map[int64]func(SeatEvent))
	}
	r.seatSubs[id] = fn
	if r.seatWatchStop == nil {
		stop := make(chan struct{})
		r.seatWatchStop = stop
		go r.watchSeats(stop)
	}
	return id
}

// UnsubscribeSeatEvents removes a subscriber. The liveness watch stops with
// the last one.
func (r *Registry) UnsubscribeSeatEvents(id int64) {
	r.seatMu.Lock()
	defer r.seatMu.Unlock()
	delete(r.seatSubs, id)
	if len(r.seatSubs) == 0 && r.seatWatchStop != nil {
		close(r.seatWatchStop)
		r.seatWatchStop = nil
	}
}

func (r *Registry) publishSeatEvent(ev SeatEvent) {
	if ev.At.IsZero() {
		ev.At = r.seatClock().Now()
	}
	r.seatMu.Lock()
	ids := make([]int64, 0, len(r.seatSubs))
	for id := range r.seatSubs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	fns := make([]func(SeatEvent), 0, len(ids))
	for _, id := range ids {
		fns = append(fns, r.seatSubs[id])
	}
	r.seatMu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

// watchSeats probes the launched seats and reports each death once. A seat
// with a lifecycle operation in flight is left for the next tick: Stop and
// Launch pass through a not-alive state that is not a death.
func (r *Registry) watchSeats(stop <-chan struct{}) {
	reported := map[string]*Agent{}
	for {
		select {
		case <-stop:
			return
		case <-r.seatClock().After(DefaultSeatWatchInterval):
		}
		r.mu.Lock()
		procs := make(map[string]*Agent, len(r.procs))
		for name, proc := range r.procs {
			if r.lifecycle[name] == nil {
				procs[name] = proc
			}
		}
		r.mu.Unlock()
		for name, proc := range reported {
			if procs[name] != proc {
				delete(reported, name)
			}
		}
		names := make([]string, 0, len(procs))
		for name := range procs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			proc := procs[name]
			if reported[name] == proc || proc.Alive() {
				continue
			}
			reported[name] = proc
			r.publishSeatEvent(SeatEvent{Kind: SeatGone, Name: name, SessionID: proc.SessionID(), Agent: proc})
		}
	}
}

// ResumeArgs configures [Registry.ResumeAll]. The zero value is usable.
type ResumeArgs struct {
	// Concurrency bounds how many seats resume at once. Zero means 2.
	Concurrency int
	// Nudge is the message sent to a seat that had to be relaunched. Empty
	// uses [DefaultRestartNudge] stamped with Now; [NoRestartNudge] sends
	// nothing. An adopted seat is never nudged: nothing happened to it.
	Nudge string
	// StartTimeout bounds one seat's launch. Zero means 90 seconds.
	StartTimeout time.Duration
	// Now stamps the default nudge. Zero uses the current time.
	Now time.Time
}

// ResumeOutcome is what happened to one seat in [Registry.ResumeAll].
type ResumeOutcome struct {
	Name string
	// How is empty when Err is set.
	How   ResumeHow
	Agent *Agent
	// OldSessionID is the conversation a reminted seat left behind.
	OldSessionID string
	Nudged       bool
	// NudgeErr is set when the seat came back but the nudge did not send.
	NudgeErr error
	// Err is set when the seat could not be brought back.
	Err error
}

// ResumeAll brings back every AutoStart seat after the process that held
// them stopped: adopt what is still running, relaunch the rest on their
// saved conversations, remint a seat whose conversation the provider
// refuses to load, and tell each relaunched seat what happened so it picks
// its work back up. Outcomes are returned in name order and published as
// seat events as they happen. Cancelling ctx stops starting further seats.
func (r *Registry) ResumeAll(ctx context.Context, args *ResumeArgs) []ResumeOutcome {
	if args == nil {
		args = &ResumeArgs{}
	}
	var names []string
	for _, def := range r.List() {
		if def.AutoStart {
			names = append(names, def.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	nudge := args.Nudge
	if nudge == "" {
		now := args.Now
		if now.IsZero() {
			now = r.seatClock().Now()
		}
		nudge = fmt.Sprintf(DefaultRestartNudge, now.Format(time.RFC3339))
	}
	conc := args.Concurrency
	if conc <= 0 {
		conc = defaultResumeConcurrency
	}
	timeout := args.StartTimeout
	if timeout <= 0 {
		timeout = grantStartTimeout
	}

	out := make([]ResumeOutcome, len(names))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, name := range names {
		out[i].Name = name
		if ctx.Err() != nil {
			out[i].Err = ctx.Err()
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.resumeSeat(ctx, &out[i], nudge, timeout)
		}()
	}
	wg.Wait()
	return out
}

func (r *Registry) resumeSeat(ctx context.Context, out *ResumeOutcome, nudge string, timeout time.Duration) {
	name := out.Name
	launch := func() (*Agent, error) {
		lctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return r.LaunchContext(lctx, name)
	}
	proc, err := r.Adopt(name)
	how := ResumeAdopted
	if err != nil {
		proc, err = launch()
		how = ResumeLaunched
	}
	if err != nil && IsCursorResumeDenied(err) {
		if old, rerr := r.remintSeat(name); rerr == nil {
			out.OldSessionID = old
			proc, err = launch()
			how = ResumeReminted
		}
	}
	if err != nil {
		out.Err = err
		r.publishSeatEvent(SeatEvent{Kind: SeatResumeFailed, Name: name, Err: err})
		return
	}
	out.How, out.Agent = how, proc
	r.publishSeatEvent(SeatEvent{Kind: SeatResumed, Name: name, SessionID: proc.SessionID(), How: how, Agent: proc})
	if how == ResumeAdopted || nudge == NoRestartNudge {
		return
	}
	if err := proc.Send(nudge); err != nil {
		out.NudgeErr = err
		return
	}
	out.Nudged = true
	r.publishSeatEvent(SeatEvent{Kind: SeatNudged, Name: name, SessionID: proc.SessionID(), Agent: proc})
}

// remintSeat points a seat whose conversation cannot be resumed at a fresh
// session, keeping everything else about its definition. It returns the
// session id left behind.
func (r *Registry) remintSeat(name string) (oldSession string, err error) {
	def := r.Def(name)
	if def == nil {
		return "", fmt.Errorf("remint %s: not registered", name)
	}
	next := *def
	next.SessionID = uuid.NewString()
	next.Materialized = false
	next.ConnectURL, next.ConnectPID = "", 0
	if err := r.Register(next); err != nil {
		return def.SessionID, err
	}
	return def.SessionID, nil
}
