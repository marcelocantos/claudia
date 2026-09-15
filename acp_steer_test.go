// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// acpSteerClient is the seat-side surface both ACP clients share for the
// 🎯T72.1 steer tests: a plain prompt, a steer, the settle path, and the
// busy answer.
type acpSteerClient interface {
	Prompt(text string) error
	Steer(ctx context.Context, text string) (string, error)
	Cancel() error
	SupersededTurnID() string
	promptInFlight() bool
	dispatchMessage(line []byte)
}

// acpWireRecorder captures every JSON-RPC message a client writes so a
// test can assert what went on the wire (a second session/prompt, and no
// session/cancel) instead of trusting the client's own account.
type acpWireRecorder struct {
	mu   sync.Mutex
	msgs []acpRPCMessage
}

func (r *acpWireRecorder) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(p)), "\n") {
		if line == "" {
			continue
		}
		var msg acpRPCMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return 0, err
		}
		r.mu.Lock()
		r.msgs = append(r.msgs, msg)
		r.mu.Unlock()
	}
	return len(p), nil
}

func (r *acpWireRecorder) Close() error { return nil }

// methods lists the methods written so far, in order.
func (r *acpWireRecorder) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.msgs))
	for _, m := range r.msgs {
		out = append(out, m.Method)
	}
	return out
}

func (r *acpWireRecorder) count(method string) int {
	n := 0
	for _, m := range r.methods() {
		if m == method {
			n++
		}
	}
	return n
}

// promptIDs returns the request ids of every session/prompt written.
func (r *acpWireRecorder) promptIDs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []int64
	for _, m := range r.msgs {
		if m.Method == "session/prompt" && m.ID != nil {
			ids = append(ids, *m.ID)
		}
	}
	return ids
}

func acpResultLine(id int64, stopReason string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"result":{"stopReason":"` + stopReason + `"}}`)
}

func itoa(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}

func acpChunkLine(sessionID, text string) []byte {
	return []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + sessionID + `","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"` + text + `"}}}}`)
}

func terminalEvents(evs []Event) []Event {
	var out []Event
	for _, ev := range evs {
		if ev.IsTerminalStop() {
			out = append(out, ev)
		}
	}
	return out
}

// Each ACP client is driven by hand: the recorder is its stdin and the
// peer's replies are dispatched directly, so the stack's behaviour is
// observed without a child process.
func acpSteerClients(t *testing.T) map[string]func(onEvent func(Event), wire *acpWireRecorder) acpSteerClient {
	t.Helper()
	return map[string]func(onEvent func(Event), wire *acpWireRecorder) acpSteerClient{
		"cursor": func(onEvent func(Event), wire *acpWireRecorder) acpSteerClient {
			return &cursorACPClient{sessionID: "s1", onEvent: onEvent, stdin: wire, pending: map[int64]chan acpRPCMessage{}}
		},
		"grok": func(onEvent func(Event), wire *acpWireRecorder) acpSteerClient {
			return &grokACPClient{sessionID: "s1", onEvent: onEvent, stdin: wire, pending: map[int64]chan acpRPCMessage{}}
		},
	}
}

// 🎯T72.1 (a)+(b)+(c)+(d): steer while in flight writes a second
// session/prompt and no session/cancel; the superseded prompt's result
// does not end the turn; the top id's result ends it exactly once; a plain
// submit while in flight is the typed busy error.
func TestACPSteerPromptIDStack(t *testing.T) {
	for name, mk := range acpSteerClients(t) {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var got []Event
			wire := &acpWireRecorder{}
			c := mk(func(ev Event) {
				mu.Lock()
				got = append(got, ev)
				mu.Unlock()
			}, wire)
			events := func() []Event {
				mu.Lock()
				defer mu.Unlock()
				return append([]Event(nil), got...)
			}

			if err := c.Prompt("write a long essay"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if c.SupersededTurnID() != "" {
				t.Fatalf("SupersededTurnID before any steer = %q, want empty", c.SupersededTurnID())
			}

			// (a) steer: second prompt on the wire, no cancel.
			mechanism, err := c.Steer(context.Background(), "stop; say STEERED")
			if err != nil {
				t.Fatalf("Steer: %v", err)
			}
			if mechanism != acpSteerMechanism {
				t.Fatalf("mechanism = %q, want %q", mechanism, acpSteerMechanism)
			}
			ids := wire.promptIDs()
			if len(ids) != 2 || ids[0] == ids[1] {
				t.Fatalf("session/prompt ids on the wire = %v, want two distinct", ids)
			}
			if n := wire.count("session/cancel"); n != 0 {
				t.Fatalf("steer wrote %d session/cancel; want none (wire: %v)", n, wire.methods())
			}
			if !c.promptInFlight() {
				t.Fatal("promptInFlight false after steer")
			}
			if c.SupersededTurnID() != itoa(ids[0]) {
				t.Fatalf("SupersededTurnID = %q, want %d", c.SupersededTurnID(), ids[0])
			}

			// (d) plain submit while in flight: typed busy, nothing written.
			err = c.Prompt("a third message")
			if !errors.Is(err, ErrTurnInFlight) {
				t.Fatalf("Prompt while in flight = %v, want errors.Is ErrTurnInFlight", err)
			}
			if !strings.Contains(err.Error(), "prompt already in flight") {
				t.Fatalf("busy error %q lost the wording jevons classifies as busy", err)
			}
			if got := wire.promptIDs(); len(got) != 2 {
				t.Fatalf("busy submit reached the wire: %v", got)
			}

			// Streaming after the steer is attributed to the top id.
			c.dispatchMessage(acpChunkLine("s1", "STEERED"))
			evs := events()
			last := evs[len(evs)-1]
			if last.Type != "assistant" || last.TurnID != itoa(ids[1]) {
				t.Fatalf("chunk after steer = %+v, want assistant on turn %d", last, ids[1])
			}

			// (b) superseded result: no terminal, still in flight; the host
			// sees a progress mark on the superseded turn instead.
			c.dispatchMessage(acpResultLine(ids[0], "cancelled"))
			if n := len(terminalEvents(events())); n != 0 {
				t.Fatalf("superseded prompt result emitted %d terminal events", n)
			}
			evs = events()
			last = evs[len(evs)-1]
			if last.Type != "progress" || last.ProgressType != ProgressPromptSuperseded || last.TurnID != itoa(ids[0]) {
				t.Fatalf("superseded result event = %+v, want progress %s on turn %d", last, ProgressPromptSuperseded, ids[0])
			}
			if !c.promptInFlight() {
				t.Fatal("superseded prompt result ended the turn")
			}

			// (c) top result: exactly one terminal, seat idle.
			c.dispatchMessage(acpResultLine(ids[1], "end_turn"))
			term := terminalEvents(events())
			if len(term) != 1 {
				t.Fatalf("terminal events after top result = %d, want 1: %+v", len(term), term)
			}
			if term[0].TurnID != itoa(ids[1]) {
				t.Fatalf("terminal TurnID = %q, want %d", term[0].TurnID, ids[1])
			}
			if c.promptInFlight() {
				t.Fatal("promptInFlight true after the top id settled")
			}

			// A late duplicate for the superseded id is not a second terminal.
			before := len(events())
			c.dispatchMessage(acpResultLine(ids[0], "end_turn"))
			if after := len(events()); after != before {
				t.Fatalf("late superseded result published %d events", after-before)
			}
			if err := c.Prompt("next turn"); err != nil {
				t.Fatalf("Prompt after settle: %v", err)
			}
		})
	}
}

// When the superseded prompt never answers (a peer that folds the turn
// without settling the old id), the top id still ends the turn and the
// stack does not leak the stale id into the next one.
func TestACPSteerTopSettlesWithoutSupersededResult(t *testing.T) {
	for name, mk := range acpSteerClients(t) {
		t.Run(name, func(t *testing.T) {
			var got []Event
			wire := &acpWireRecorder{}
			c := mk(func(ev Event) { got = append(got, ev) }, wire)
			if err := c.Prompt("one"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Steer(context.Background(), "two"); err != nil {
				t.Fatal(err)
			}
			ids := wire.promptIDs()
			c.dispatchMessage(acpResultLine(ids[1], "end_turn"))
			if len(terminalEvents(got)) != 1 || c.promptInFlight() {
				t.Fatalf("top result: terminals=%d inFlight=%v", len(terminalEvents(got)), c.promptInFlight())
			}
			c.dispatchMessage(acpResultLine(ids[0], "end_turn"))
			if len(terminalEvents(got)) != 1 {
				t.Fatalf("stale superseded result ended a turn: %d terminals", len(terminalEvents(got)))
			}
		})
	}
}

// Steer on an idle session is a plain submit and says so.
func TestACPSteerIdleIsSubmit(t *testing.T) {
	for name, mk := range acpSteerClients(t) {
		t.Run(name, func(t *testing.T) {
			wire := &acpWireRecorder{}
			c := mk(nil, wire)
			mechanism, err := c.Steer(context.Background(), "hello")
			if err != nil {
				t.Fatal(err)
			}
			if mechanism != acpSubmitMechanism {
				t.Fatalf("mechanism = %q, want %q", mechanism, acpSubmitMechanism)
			}
			if !c.promptInFlight() || len(wire.promptIDs()) != 1 {
				t.Fatalf("idle steer: inFlight=%v prompts=%v", c.promptInFlight(), wire.promptIDs())
			}
			if c.SupersededTurnID() != "" {
				t.Fatalf("idle steer superseded %q", c.SupersededTurnID())
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := c.Steer(ctx, "x"); !errors.Is(err, context.Canceled) {
				t.Fatalf("Steer with cancelled ctx = %v", err)
			}
		})
	}
}

// Interrupt stays session/cancel and clears the whole stack, steered or not.
func TestACPSteerThenCancelClearsStack(t *testing.T) {
	for name, mk := range acpSteerClients(t) {
		t.Run(name, func(t *testing.T) {
			wire := &acpWireRecorder{}
			c := mk(nil, wire)
			if err := c.Prompt("one"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Steer(context.Background(), "two"); err != nil {
				t.Fatal(err)
			}
			if err := c.Cancel(); err != nil {
				t.Fatal(err)
			}
			if wire.count("session/cancel") != 1 {
				t.Fatalf("Cancel wire = %v, want one session/cancel", wire.methods())
			}
			if c.promptInFlight() {
				t.Fatal("Cancel left the turn in flight")
			}
		})
	}
}

func TestACPPromptStack(t *testing.T) {
	var s acpPromptStack
	if s.inFlight() || s.top() != 0 || s.settle(1) != acpPromptNotOurs {
		t.Fatal("empty stack is not idle")
	}
	s.push(1)
	s.push(2)
	s.push(3)
	if s.top() != 3 || s.supersededTurnID() != "2" {
		t.Fatalf("top=%d superseded=%q", s.top(), s.supersededTurnID())
	}
	if got := s.settle(1); got != acpPromptSuperseded {
		t.Fatalf("settle(1) = %v, want superseded", got)
	}
	if got := s.settle(1); got != acpPromptNotOurs {
		t.Fatalf("settle(1) twice = %v, want not ours", got)
	}
	if got := s.settle(3); got != acpPromptTurnDone || s.inFlight() {
		t.Fatalf("settle(top) = %v inFlight=%v", got, s.inFlight())
	}
	if got := s.settle(2); got != acpPromptNotOurs {
		t.Fatalf("settle after the turn ended = %v, want not ours", got)
	}
}

// acpSteerRun drives one started client through prompt → first chunk →
// steer → terminal and reports what was observed. Shared by the fake-peer
// hermetics and the live smokes.
type acpSteerRun struct {
	events   []Event
	wire     *acpWireRecorder
	steerID  string
	firstID  string
	mechanic string
}

func (r *acpSteerRun) text() string {
	var b strings.Builder
	// Chunks are token fragments (PreviewUpdateAppend); join them raw.
	for _, ev := range r.events {
		if ev.Type == "assistant" {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func driveACPSteer(t *testing.T, ctx context.Context, c acpSteerClient, collected func() []Event, wire *acpWireRecorder, first, steer string) *acpSteerRun {
	t.Helper()
	waitFor := func(what string, pred func([]Event) bool) []Event {
		t.Helper()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			evs := collected()
			if pred(evs) {
				return evs
			}
			select {
			case <-ctx.Done():
				t.Fatalf("timeout waiting for %s; events so far: %d", what, len(evs))
			case <-tick.C:
			}
		}
	}
	if err := c.Prompt(first); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	waitFor("first assistant chunk", func(evs []Event) bool {
		for _, ev := range evs {
			if ev.Type == "assistant" && ev.Text != "" {
				return true
			}
		}
		return false
	})
	if !c.promptInFlight() {
		t.Fatal("first prompt already settled before the steer could be tried")
	}
	mechanism, err := c.Steer(ctx, steer)
	if err != nil {
		t.Fatalf("Steer: %v", err)
	}
	evs := waitFor("terminal event", func(evs []Event) bool { return len(terminalEvents(evs)) > 0 })
	// Give a late superseded result a moment to surface so the
	// exactly-once assertion sees it.
	time.Sleep(200 * time.Millisecond)
	evs = collected()
	ids := wire.promptIDs()
	run := &acpSteerRun{events: evs, wire: wire, mechanic: mechanism}
	if len(ids) >= 2 {
		run.firstID, run.steerID = itoa(ids[0]), itoa(ids[1])
	}
	return run
}

func assertACPSteerRun(t *testing.T, run *acpSteerRun, marker string) {
	t.Helper()
	if run.mechanic != acpSteerMechanism {
		t.Errorf("mechanism = %q, want %q", run.mechanic, acpSteerMechanism)
	}
	if n := run.wire.count("session/cancel"); n != 0 {
		t.Errorf("session/cancel on the wire %d times; steer must not cancel (%v)", n, run.wire.methods())
	}
	if n := run.wire.count("session/prompt"); n != 2 {
		t.Errorf("session/prompt on the wire %d times, want 2", n)
	}
	term := terminalEvents(run.events)
	if len(term) != 1 {
		t.Errorf("terminal events = %d, want exactly one", len(term))
	} else if term[0].TurnID != run.steerID {
		t.Errorf("terminal TurnID = %q, want the steer id %q", term[0].TurnID, run.steerID)
	}
	if !strings.Contains(strings.ToUpper(run.text()), strings.ToUpper(marker)) {
		t.Errorf("reply did not change direction: no %q in %q", marker, run.text())
	}
}

// Whole-client hermetic on the fake peers: the child holds the first
// prompt, folds the steer in, settles the old id as cancelled and the new
// id as end_turn.
func TestHermeticACPSteerMidTurn(t *testing.T) {
	t.Run("cursor", func(t *testing.T) {
		bin := writeFakeCursorACP(t)
		t.Setenv("FAKE_ACP_STEER", "1")
		var mu sync.Mutex
		var got []Event
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		c, err := startCursorACP(ctx, bin, t.TempDir(), "", "", false, nil, nil, func(ev Event) {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		wire := &acpWireRecorder{}
		c.mu.Lock()
		c.stdin = teeWriteCloser{c.stdin, wire}
		c.mu.Unlock()
		run := driveACPSteer(t, ctx, c, func() []Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]Event(nil), got...)
		}, wire, "write a long essay", "stop; reply STEERED")
		assertACPSteerRun(t, run, "STEERED")
	})
	t.Run("grok", func(t *testing.T) {
		bin := writeFakeGrokACP(t)
		t.Setenv("FAKE_ACP_STEER", "1")
		var mu sync.Mutex
		var got []Event
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		c, err := startGrokACP(bin, t.TempDir(), "", "", false, nil, nil, func(ev Event) {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		wire := &acpWireRecorder{}
		c.mu.Lock()
		c.stdin = teeWriteCloser{c.stdin, wire}
		c.mu.Unlock()
		run := driveACPSteer(t, ctx, c, func() []Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]Event(nil), got...)
		}, wire, "write a long essay", "stop; reply STEERED")
		assertACPSteerRun(t, run, "STEERED")
	})
}

// teeWriteCloser records what a client writes to its peer without
// changing the peer's view of the bytes.
type teeWriteCloser struct {
	w    interface{ Write([]byte) (int, error) }
	wire *acpWireRecorder
}

func (t teeWriteCloser) Write(p []byte) (int, error) {
	if _, err := t.wire.Write(p); err != nil {
		return 0, err
	}
	return t.w.Write(p)
}

func (t teeWriteCloser) Close() error {
	if c, ok := t.w.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

const acpLiveSteerFirst = "Write a 400-word story about a lighthouse keeper, one sentence per line, without using any tools. Do not stop early."

const acpLiveSteerText = "Stop the story right now. Reply with exactly the single word STEERED and nothing else."

// TestCursorSessionLiveSmokeSteer is the 🎯T72.1 owner acceptance on the
// real Cursor agent: a mid-turn steer changes the reply's direction, with
// no session/cancel on the wire. Opt-in (spends credit). The name extends
// TestCursorSessionLiveSmoke so `make live` selects it.
func TestCursorSessionLiveSmokeSteer(t *testing.T) {
	if os.Getenv("CLAUDIA_CURSOR_LIVE") == "" {
		t.Skip("CLAUDIA_CURSOR_LIVE not set (this test spends API credit)")
	}
	bin, err := resolveCursorBin()
	if err != nil {
		t.Skipf("cursor agent binary not found: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var mu sync.Mutex
	var got []Event
	c, err := startCursorACP(ctx, bin, t.TempDir(), "", "", false, nil, nil, func(ev Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	wire := &acpWireRecorder{}
	c.mu.Lock()
	c.stdin = teeWriteCloser{c.stdin, wire}
	c.mu.Unlock()
	run := driveACPSteer(t, ctx, c, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), got...)
	}, wire, acpLiveSteerFirst, acpLiveSteerText)
	logACPSteerRun(t, run)
	assertACPSteerRun(t, run, "STEERED")
}

// TestGrokSessionLiveSmokeSteer is the Grok wire for the same change
// (finish-slice policy: Grok may complete its current slice first).
func TestGrokSessionLiveSmokeSteer(t *testing.T) {
	if os.Getenv("CLAUDIA_GROK_LIVE") == "" {
		t.Skip("CLAUDIA_GROK_LIVE not set (this test spends API credit)")
	}
	bin, err := resolveGrokBin()
	if err != nil {
		t.Skipf("grok binary not found: %v", err)
	}
	t.Setenv(EnvGrokConnect, "")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var mu sync.Mutex
	var got []Event
	c, err := startGrokACP(bin, t.TempDir(), "", "", false, nil, nil, func(ev Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	wire := &acpWireRecorder{}
	c.mu.Lock()
	c.stdin = teeWriteCloser{c.stdin, wire}
	c.mu.Unlock()
	run := driveACPSteer(t, ctx, c, func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), got...)
	}, wire, acpLiveSteerFirst, acpLiveSteerText)
	logACPSteerRun(t, run)
	assertACPSteerRun(t, run, "STEERED")
}

func logACPSteerRun(t *testing.T, run *acpSteerRun) {
	t.Helper()
	t.Logf("wire: %v", run.wire.methods())
	t.Logf("first prompt id %s, steer id %s, mechanism %s", run.firstID, run.steerID, run.mechanic)
	for _, ev := range run.events {
		if ev.IsTerminalStop() || ev.ProgressType == ProgressPromptAccepted || ev.ProgressType == ProgressPromptSuperseded {
			t.Logf("event type=%s turn=%s stop=%s progress=%s raw=%s", ev.Type, ev.TurnID, ev.StopReason, ev.ProgressType, string(ev.Raw))
		}
	}
	text := run.text()
	if len(text) > 600 {
		text = text[:300] + " … " + text[len(text)-300:]
	}
	t.Logf("assistant text: %q", text)
}
