// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"strconv"
	"testing"
)

func closedReady() chan struct{} { ch := make(chan struct{}); close(ch); return ch }

// Real in-process ACP transport: the abandoned delivery answers after the
// silence-triggered reissue, while the host only supplied one RequestID.
func TestRequestIDCursorRedeemedReissue(t *testing.T) {
	shortenCursorSilenceBound(t, cursorHermeticPeerBound)
	a := &Agent{provider: ProviderCursor, alive: true, ready: closedReady(), eventSubs: map[int64]EventFunc{}}
	events := make(chan Event, 32)
	a.eventSubs[1] = func(ev Event) { events <- ev }
	c, peer := pipedCursorClient(t, a.publishEvent)
	a.ops.send = func(*Agent, string) error { return c.Prompt("pong") }
	done := make(chan error, 1)
	go func() { done <- a.SendWithRequestID("pong", "host-request-7") }()
	first := peer.nextPrompt()
	second := peer.nextPrompt()
	if first == second {
		t.Fatal("reissue reused provider id")
	}
	peer.chunk("pong")
	peer.result(first, "end_turn")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var terminal Event
	for {
		ev := <-events
		if ev.IsTerminalStop() {
			terminal = ev
			break
		}
	}
	if terminal.TurnID != strconv.FormatInt(second, 10) {
		t.Fatalf("terminal was not published under the surviving id: %+v", terminal)
	}
	if terminal.RequestID != "host-request-7" {
		t.Fatalf("redeemed event: %+v", terminal)
	}
	a.mu.Lock()
	remaining := len(a.requestByTurn)
	a.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("request binding not closed: %d", remaining)
	}
}

func TestRequestIDTerminalNormalizationDoesNotReset(t *testing.T) {
	for _, status := range []string{"interrupted", "completed"} {
		t.Run("codex-"+status, func(t *testing.T) {
			a := &Agent{provider: ProviderCodex, alive: true, ready: closedReady(), eventSubs: map[int64]EventFunc{}}
			var got []Event
			a.eventSubs[1] = func(ev Event) { got = append(got, ev) }
			a.ops.send = func(a *Agent, _ string) error {
				a.publishEvent(Event{Type: "progress", TurnID: "codex-turn", ProgressType: ProgressPromptAccepted})
				return nil
			}
			if err := a.SendWithRequestID("hi", "host-codex"); err != nil {
				t.Fatal(err)
			}
			ev, ok := (codexAppServerEvent{Method: "turn/completed", TurnID: "codex-turn", Status: status}).agentEvent()
			if !ok {
				t.Fatal("normalization dropped event")
			}
			a.publishEvent(ev)
			if got[len(got)-1].RequestID != "host-codex" || got[len(got)-1].StopReason != "end_turn" {
				t.Fatalf("normalized: %+v", got[len(got)-1])
			}
		})
	}
	for _, status := range []string{"cancelled", "refusal"} {
		t.Run("grok-"+status, func(t *testing.T) {
			a := &Agent{provider: ProviderGrok, alive: true, ready: closedReady(), eventSubs: map[int64]EventFunc{}}
			var got []Event
			a.eventSubs[1] = func(ev Event) { got = append(got, ev) }
			a.ops.send = func(a *Agent, _ string) error {
				a.publishEvent(acpPromptAcceptedEvent("session", 42))
				return nil
			}
			if err := a.SendWithRequestID("hi", "host-grok"); err != nil {
				t.Fatal(err)
			}
			c := &grokACPClient{onEvent: a.publishEvent}
			c.publishPromptResult(acpRPCMessage{Result: json.RawMessage(`{"stopReason":"` + status + `"}`)}, "session", "42")
			if got[len(got)-1].RequestID != "host-grok" || got[len(got)-1].StopReason != "end_turn" {
				t.Fatalf("normalized: %+v", got[len(got)-1])
			}
		})
	}
}

func TestRequestIDLegacySendStaysEmpty(t *testing.T) {
	a := &Agent{provider: ProviderGrok, alive: true, ready: closedReady(), eventSubs: map[int64]EventFunc{}}
	var events []Event
	a.eventSubs[1] = func(ev Event) { events = append(events, ev) }
	a.ops.send = func(a *Agent, _ string) error {
		a.publishEvent(acpPromptAcceptedEvent("session", 5))
		a.publishEvent(Event{Type: "assistant", TurnID: "5", Text: "ok", StopReason: "end_turn"})
		return nil
	}
	if err := a.Send("legacy"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.RequestID != "" {
			t.Fatalf("legacy event gained request: %+v", ev)
		}
	}
}

func TestRequestIDWireRoundTrip(t *testing.T) {
	in := Event{Type: "assistant", TurnID: "provider", RequestID: "host", StopReason: "end_turn"}
	raw, err := EncodeEventWire(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeEventWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestID != in.RequestID {
		t.Fatalf("round trip: %+v", out)
	}
}

func TestRequestIDCodexSteerSameProviderTurn(t *testing.T) {
	a := &Agent{provider: ProviderCodex, alive: true, ready: closedReady(), eventSubs: map[int64]EventFunc{}}
	var got Event
	a.eventSubs[1] = func(ev Event) {
		if ev.IsTerminalStop() {
			got = ev
		}
	}
	a.ops.send = func(a *Agent, _ string) error {
		a.publishEvent(Event{Type: "progress", TurnID: "codex-1", ProgressType: ProgressPromptAccepted})
		return nil
	}
	a.ops.turnPhase = func(*Agent) TurnPhase { return TurnInTurn }
	a.ops.steer = func(*Agent, string) (DeliveryOutcome, error) {
		return DeliveryOutcome{Mechanism: MechanismCodexTurnSteer}, nil
	}
	if err := a.SendWithRequestID("original", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SendModeWithRequestID("followup", DeliverySteer, "second"); err != nil {
		t.Fatal(err)
	}
	ev, _ := (codexAppServerEvent{Method: "turn/completed", TurnID: "codex-1", Status: "interrupted"}).agentEvent()
	a.publishEvent(ev)
	if got.RequestID != "second" {
		t.Fatalf("same-turn steer lost host request: %+v", got)
	}
}

// The ACP clients construct tool events and provisional chunks on different
// branches of handleSessionUpdate. Exercise those branches through the real
// client routing into Agent.publishEvent, rather than fabricating Events that
// would only test the final map lookup.
func TestRequestIDACPToolAndProvisionalEvents(t *testing.T) {
	for _, backend := range []struct {
		name     string
		provider Provider
		updates  func(onEvent func(Event)) (send func(), update func(json.RawMessage))
	}{
		{"cursor", ProviderCursor, func(onEvent func(Event)) (func(), func(json.RawMessage)) {
			c := &cursorACPClient{sessionID: "session", onEvent: onEvent}
			return func() {
				c.mu.Lock()
				c.prompts.push(17)
				c.mu.Unlock()
				publishEvent(c.onEvent, acpPromptAcceptedEvent("session", 17))
			}, c.handleSessionUpdate
		}},
		{"grok", ProviderGrok, func(onEvent func(Event)) (func(), func(json.RawMessage)) {
			c := &grokACPClient{sessionID: "session", onEvent: onEvent}
			return func() {
				c.mu.Lock()
				c.prompts.push(17)
				c.mu.Unlock()
				publishEvent(c.onEvent, acpPromptAcceptedEvent("session", 17))
			}, c.handleSessionUpdate
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			a := &Agent{provider: backend.provider, alive: true, ready: closedReady(), eventSubs: map[int64]EventFunc{}}
			var got []Event
			a.eventSubs[1] = func(ev Event) { got = append(got, ev) }
			send, update := backend.updates(a.publishEvent)
			a.ops.send = func(*Agent, string) error { send(); return nil }
			if err := a.SendWithRequestID("use a tool", "host-tool-request"); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct{ name, body, progress, preview string }{
				{"tool_call", `{"sessionId":"session","update":{"sessionUpdate":"tool_call","toolCallId":"tool-1","title":"Bash","status":"pending"}}`, ProgressToolUse, ""},
				{"tool_call_update", `{"sessionId":"session","update":{"sessionUpdate":"tool_call_update","toolCallId":"tool-1","title":"Bash","status":"completed"}}`, ProgressToolUse, ""},
				{"provisional", `{"sessionId":"session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"streamed"}}}`, "", PreviewUpdateAppend},
			} {
				t.Run(tc.name, func(t *testing.T) {
					update(json.RawMessage(tc.body))
					ev := got[len(got)-1]
					if ev.RequestID != "host-tool-request" || ev.TurnID != "17" || ev.ProgressType != tc.progress || ev.PreviewUpdate != tc.preview {
						t.Fatalf("event lost request correlation or kind: %+v", ev)
					}
					if tc.progress == ProgressToolUse && (ev.ToolCallID != "tool-1" || ev.ToolTitle != "Bash") {
						t.Fatalf("tool identity lost: %+v", ev)
					}
					if tc.preview != "" && (ev.Type != "assistant" || ev.Text != "streamed") {
						t.Fatalf("provisional text lost: %+v", ev)
					}
					wire, err := EncodeEventWire(ev)
					if err != nil {
						t.Fatal(err)
					}
					decoded, err := DecodeEventWire(wire)
					if err != nil {
						t.Fatal(err)
					}
					if decoded.RequestID != "host-tool-request" {
						t.Fatalf("broker stream lost request on %s: %+v", tc.name, decoded)
					}
				})
			}
		})
	}
}
