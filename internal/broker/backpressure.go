// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// SignalKind classifies one backpressure observation.
type SignalKind int

const (
	// SignalUnknown is the zero value: a line that carries no backpressure
	// verdict either way. Policy must ignore it rather than treat it as OK.
	SignalUnknown SignalKind = iota
	// SignalOK is a turn that completed with no backpressure — the AIMD
	// controller's additive-increase trigger.
	SignalOK
	// SignalRateLimited is an HTTP 429 / rate_limit_error — the AIMD
	// controller's multiplicative-decrease trigger.
	SignalRateLimited
	// SignalOverloaded is an HTTP 529 / overloaded_error. Distinct from 429:
	// it is server capacity, not this account's quota, so a controller may
	// choose to back off without lowering its own cap.
	SignalOverloaded
)

// String renders the kind for test failures and logs.
func (k SignalKind) String() string {
	switch k {
	case SignalOK:
		return "ok"
	case SignalRateLimited:
		return "rate_limited"
	case SignalOverloaded:
		return "overloaded"
	default:
		return "unknown"
	}
}

// Signal is one backpressure observation delivered to broker policy.
type Signal struct {
	// Kind is what the upstream said.
	Kind SignalKind
	// At is when the observation was made, stamped from the injected Clock
	// (never from the wall clock) so replayed tapes are reproducible.
	At time.Time
	// Status is the HTTP status when the upstream reported one, else 0.
	Status int
	// RetryAfter is the upstream's requested cooldown, or 0 when absent.
	// Policy must treat 0 as "no hint", not as "retry immediately".
	RetryAfter time.Duration
}

// BackpressureSource is the broker's sole source of rate-limit information.
// Policy code (AIMD concurrency control, pool drain, preemption) consumes
// Signals from this channel and never parses an HTTP response, socket, or JSONL
// stream itself — that separation is what lets the AIMD oracle replay a seeded
// 429 tape deterministically under `go test -race` with zero API credit.
// policy_guard_test.go fails the build if a policy path reaches for a live
// socket instead.
type BackpressureSource interface {
	// Signals returns the stream of observations. It is closed when the
	// underlying source is exhausted.
	Signals() <-chan Signal
}

// ScriptedBackpressure is a BackpressureSource that replays a fixed tape of
// signals, stamping each with the injected Clock's current time. It is the test
// double the AIMD simulator (T2.2) drives: a tape such as
// {OK, OK, RateLimited, OK, ...} plus a ManualClock makes never-exceed-cap,
// halve-on-429 and additive-recover decidable without any network.
type ScriptedBackpressure struct {
	clock Clock
	ch    chan Signal

	mu   sync.Mutex
	tape []Signal
}

// NewScriptedBackpressure returns a source that will emit the given kinds, in
// order, one per Emit call. Nothing is delivered until Emit runs, so the test
// controls interleaving with clock advances exactly.
func NewScriptedBackpressure(clock Clock, tape ...Signal) *ScriptedBackpressure {
	return &ScriptedBackpressure{
		clock: clock,
		ch:    make(chan Signal, len(tape)),
		tape:  append([]Signal(nil), tape...),
	}
}

// Signals returns the replay channel.
func (s *ScriptedBackpressure) Signals() <-chan Signal { return s.ch }

// Emit delivers the next signal on the tape, stamped with the clock's current
// time, and reports whether one was available. When the tape is exhausted it
// closes the channel so a consuming policy loop terminates.
func (s *ScriptedBackpressure) Emit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tape) == 0 {
		return false
	}
	next := s.tape[0]
	s.tape = s.tape[1:]
	next.At = s.clock.Now()
	s.ch <- next
	if len(s.tape) == 0 {
		close(s.ch)
	}
	return true
}

// EmitAll drains the whole tape, which is what a simulator does when it does
// not need to interleave clock advances between signals.
func (s *ScriptedBackpressure) EmitAll() {
	for s.Emit() {
	}
}

// errorEnvelope is the subset of a claude JSONL result line that carries a
// backpressure verdict. Everything else on the line is deliberately ignored.
type errorEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Error   struct {
		Type       string  `json:"type"`
		Status     int     `json:"status"`
		RetryAfter float64 `json:"retry_after"`
	} `json:"error"`
}

// Backpressure HTTP statuses, named so policy and the classifier never spell
// the numbers inline.
const (
	statusRateLimited = 429
	statusOverloaded  = 529
)

// ClassifyResultLine is the pure decision that turns one line of claude's JSONL
// output into a backpressure Signal, stamped at now. ok is false when the line
// carries no verdict (an assistant turn, a usage block, a blank line), which
// policy must skip rather than count as success.
//
// It is pure and total by design: it is the single place the wire shape of a
// 429 is interpreted, so the fake claude in brokertest can manufacture that
// referent exactly and the classifier can be table-tested against it.
func ClassifyResultLine(line []byte, now time.Time) (Signal, bool) {
	var env errorEnvelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Signal{}, false
	}
	if env.Type != "result" {
		return Signal{}, false
	}
	sig := Signal{At: now, Status: env.Error.Status}
	if env.Error.RetryAfter > 0 {
		sig.RetryAfter = time.Duration(env.Error.RetryAfter * float64(time.Second))
	}
	switch {
	case env.Error.Type == "rate_limit_error" || env.Error.Status == statusRateLimited:
		sig.Kind = SignalRateLimited
		if sig.Status == 0 {
			sig.Status = statusRateLimited
		}
	case env.Error.Type == "overloaded_error" || env.Error.Status == statusOverloaded:
		sig.Kind = SignalOverloaded
		if sig.Status == 0 {
			sig.Status = statusOverloaded
		}
	case env.IsError:
		// An error that is not backpressure (auth, bad request). It carries no
		// verdict about capacity, so policy must not treat it as either.
		return Signal{}, false
	default:
		sig.Kind = SignalOK
	}
	return sig, true
}

// JSONLBackpressure is the production BackpressureSource: it reads claude's
// JSONL stream, classifies each line with ClassifyResultLine, and publishes the
// verdicts. Policy sees only the Signal channel, so swapping in
// ScriptedBackpressure for a test changes nothing on the policy side.
type JSONLBackpressure struct {
	ch chan Signal
}

// NewJSONLBackpressure starts a goroutine reading JSONL from r, stamping
// signals from clock. The channel closes when r is exhausted.
func NewJSONLBackpressure(clock Clock, r io.Reader) *JSONLBackpressure {
	b := &JSONLBackpressure{ch: make(chan Signal)}
	go func() {
		defer close(b.ch)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			if sig, ok := ClassifyResultLine(sc.Bytes(), clock.Now()); ok {
				b.ch <- sig
			}
		}
	}()
	return b
}

// Signals returns the classified stream.
func (b *JSONLBackpressure) Signals() <-chan Signal { return b.ch }

// Compile-time proof that both sources satisfy the interface.
var (
	_ BackpressureSource = (*ScriptedBackpressure)(nil)
	_ BackpressureSource = (*JSONLBackpressure)(nil)
)
