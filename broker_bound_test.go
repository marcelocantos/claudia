// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

// Oracles for bounding a provider payload before it reaches the broker wire
// (🎯T73). The provider scanner allows 16 MiB and the broker frame allows 1,
// so a screenshot tool_result is a legal provider line and an impossible
// broker frame. These pin that the relayed event fits, still parses, and says
// that it was bounded.

// screenshotToolResult is the payload that caused the incident: a Claude
// transcript line carrying base64 image data in a tool_result. Two blocks on
// one line is the acceptance's shape, and it is not contrived — a turn that
// screenshots twice produces exactly this.
func screenshotToolResult(t *testing.T, blocks, size int) []byte {
	t.Helper()
	img := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG\r\n\x1a\n", size/8)))
	content := make([]any, 0, blocks)
	for range blocks {
		content = append(content, map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "base64", "media_type": "image/png", "data": img},
		})
	}
	line, err := json.Marshal(map[string]any{
		"type":       "user",
		"uuid":       "rec-1",
		"sessionId":  "sid-1",
		"inputChars": 1234567890123,
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "toolu_1", "content": content,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// frameFor is the event as the daemon would put it on the wire: encoded,
// wrapped in the agent_event response, and framed. Its length is what the
// broker's line cap actually applies to, so it is what the budget must be
// measured against — not the payload alone.
func frameFor(t *testing.T, ev Event) (raw json.RawMessage, frame int) {
	t.Helper()
	raw, err := EncodeEventWire(ev)
	if err != nil {
		t.Fatal(err)
	}
	line, err := (&broker.Response{
		Type:       broker.TypeAgentEvent,
		AgentEvent: &broker.AgentEventMessage{Name: "cl-worker-1", Event: raw},
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return raw, len(line) + 1 // +1 for the newline the framing appends
}

// TestOversizedToolResultIsRelayedBoundedAndFits is the acceptance oracle: a
// ~600 KB base64 tool_result appearing twice on one line is relayed, it fits
// one frame, and the consumer can tell it was bounded.
func TestOversizedToolResultIsRelayedBoundedAndFits(t *testing.T) {
	line := screenshotToolResult(t, 2, 600<<10)
	if len(line) <= broker.MaxLineLen {
		t.Fatalf("the fixture is not oversized: %d bytes", len(line))
	}
	ev := Event{Type: "user", SessionID: "sid-1", RecordID: "rec-1", Raw: line}

	// Unbounded, this is the incident: Raw is []byte, which JSON renders as
	// base64 and a third larger again, so the frame is far past the cap and
	// the wire would refuse it. Encoding without the bound is what the
	// codec did before 🎯T73, and it is the control this test needs — without
	// it, a fixture that quietly stopped being oversized would pass.
	unbounded, err := json.Marshal(eventWire(ev))
	if err != nil {
		t.Fatal(err)
	}
	if len(unbounded) <= broker.MaxLineLen {
		t.Fatalf("fixture does not reproduce the defect: unbounded payload is only %d bytes", len(unbounded))
	}

	raw, frame := frameFor(t, ev)
	if frame > broker.MaxLineLen {
		t.Fatalf("the relayed frame is %d bytes, over the %d-byte cap", frame, broker.MaxLineLen)
	}

	back, err := DecodeEventWire(raw)
	if err != nil {
		t.Fatalf("a bounded event no longer decodes: %v", err)
	}
	if !back.Truncated {
		t.Error("a bounded event must say so: Truncated is false")
	}
	if back.Type != "user" || back.SessionID != "sid-1" || back.RecordID != "rec-1" {
		t.Errorf("bounding lost the event's identity: %+v", back)
	}

	// The payload still parses, still has its shape, and names what it lost.
	var doc struct {
		Type    string `json:"type"`
		UUID    string `json:"uuid"`
		Message struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   []struct {
					Type   string `json:"type"`
					Source struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(back.Raw, &doc); err != nil {
		t.Fatalf("the bounded payload is not valid JSON: %v", err)
	}
	if doc.Type != "user" || doc.UUID != "rec-1" {
		t.Errorf("bounding rewrote the payload's own fields: %+v", doc)
	}
	blocks := doc.Message.Content
	if len(blocks) != 1 || blocks[0].ToolUseID != "toolu_1" || len(blocks[0].Content) != 2 {
		t.Fatalf("bounding lost the tool_result structure: %+v", blocks)
	}
	for i, img := range blocks[0].Content {
		if img.Source.MediaType != "image/png" {
			t.Errorf("block %d lost its media type: %+v", i, img.Source)
		}
		if !IsWireElided(img.Source.Data) {
			t.Errorf("block %d was not elided: %.80q", i, img.Source.Data)
		}
	}
}

// TestBoundedFramesAreAcceptedByTheWire closes the loop: the frame the codec
// produces is one the transport will actually write. A budget that only the
// test believes in is not a bound.
func TestBoundedFramesAreAcceptedByTheWire(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event Event
	}{
		{"two 600 KB images", Event{Type: "user", Raw: screenshotToolResult(t, 2, 600<<10)}},
		{"one 8 MiB image", Event{Type: "user", Raw: screenshotToolResult(t, 1, 8<<20)}},
		{"twenty 600 KB images", Event{Type: "user", Raw: screenshotToolResult(t, 20, 600<<10)}},
		{"a 4 MiB assistant text", Event{Type: "assistant", Text: strings.Repeat("t", 4<<20)}},
		{"a payload that is not JSON", Event{Type: "system", Raw: []byte(strings.Repeat("~", 4<<20))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, frame := frameFor(t, tc.event)
			if frame > broker.MaxLineLen {
				t.Fatalf("frame is %d bytes, over the %d-byte cap", frame, broker.MaxLineLen)
			}
			back, err := DecodeEventWire(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !back.Truncated {
				t.Error("an event that was bounded must say so")
			}
		})
	}
}

// TestAnEventWithinBudgetIsRelayedByteForByte is the other half of the bound:
// the ordinary event — which is every event — must be untouched, and must not
// grow a truncated flag on the wire.
func TestAnEventWithinBudgetIsRelayedByteForByte(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"u-1","message":{"content":[{"type":"text","text":"hi"}]}}`)
	ev := Event{Type: "assistant", Text: "hi", Raw: line, MessageID: "m-1"}
	raw, err := EncodeEventWire(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"truncated"`) {
		t.Errorf("an untouched event carries a truncation flag: %s", raw)
	}
	back, err := DecodeEventWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Truncated {
		t.Error("an untouched event reports itself truncated")
	}
	if string(back.Raw) != string(line) {
		t.Errorf("the payload was rewritten:\n got %s\nwant %s", back.Raw, line)
	}
}

// TestBoundWirePayloadKeepsNumbersExact pins the one way a re-encode could
// corrupt a payload it was only meant to shrink: decoding into any turns every
// number into a float64, and a 13-digit id comes back as 1.234567890123e+12.
func TestBoundWirePayloadKeepsNumbersExact(t *testing.T) {
	line := screenshotToolResult(t, 2, 600<<10)
	bounded, cut := BoundWirePayload(line)
	if !cut {
		t.Fatal("an oversized payload was not bounded")
	}
	var doc struct {
		InputChars json.Number `json:"inputChars"`
	}
	if err := json.Unmarshal(bounded, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.InputChars.String() != "1234567890123" {
		t.Errorf("a number was re-encoded as %s", doc.InputChars)
	}
}

// TestBoundWirePayloadLeavesSmallLinesAlone keeps the common path free of a
// re-encode nobody asked for: byte-for-byte, key order included.
func TestBoundWirePayloadLeavesSmallLinesAlone(t *testing.T) {
	line := []byte(`{"z":1,"a":{"nested":[1,2,3]},"m":"text"}`)
	got, cut := BoundWirePayload(line)
	if cut {
		t.Error("a small payload was reported as bounded")
	}
	if string(got) != string(line) {
		t.Errorf("a small payload was re-encoded:\n got %s\nwant %s", got, line)
	}
}

// TestOversizedTaskEventToolInputIsBounded is the same contract on the task
// path, where a screenshot arrives as the JSON tool input.
func TestOversizedTaskEventToolInputIsBounded(t *testing.T) {
	ev := TaskEvent{
		Type: TaskEventToolUse, ToolName: "Read", ToolID: "toolu_1",
		ToolInput: string(screenshotToolResult(t, 2, 600<<10)),
	}
	raw, err := EncodeTaskEventWire(ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > broker.MaxLineLen {
		t.Fatalf("the relayed task event is %d bytes, over the %d-byte cap", len(raw), broker.MaxLineLen)
	}
	back, err := DecodeTaskEventWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Truncated {
		t.Error("a bounded task event must say so")
	}
	if back.ToolName != "Read" || back.ToolID != "toolu_1" {
		t.Errorf("bounding lost the tool call's identity: %+v", back)
	}
	if !json.Valid([]byte(back.ToolInput)) {
		t.Error("the bounded tool input is not valid JSON")
	}
}

// TestIsWireElidedDoesNotClaimProviderText keeps the marker check narrow: it
// must not report a string the provider really sent as an elision.
func TestIsWireElidedDoesNotClaimProviderText(t *testing.T) {
	for _, s := range []string{
		"", "hello", "[claudia: elided]",
		"the agent said [claudia: elided 1 bytes, too large for the broker wire]",
	} {
		if IsWireElided(s) {
			t.Errorf("IsWireElided claimed %q", s)
		}
	}
	if !IsWireElided(elidedMarker(612345)) {
		t.Error("IsWireElided does not recognise its own marker")
	}
}

// manySmallStrings is a provider line whose size is in its shape, not in any
// one value: every string in it is well under maxWireStringBytes, so no
// elision reaches any of them. It is the payload that lands in the gap
// between the budget and the frame ceiling.
func manySmallStrings(t *testing.T, n, each int) []byte {
	t.Helper()
	rows := make([]any, 0, n)
	for i := range n {
		rows = append(rows, map[string]any{
			"i": i, "line": strings.Repeat("s", each),
		})
	}
	line, err := json.Marshal(map[string]any{
		"type": "user", "uuid": "rec-2", "rows": rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// TestAPayloadOverBudgetButUnderTheCapIsRelayedWhole pins the gap between the
// budget bounding aims for and the size the wire actually refuses.
//
// Bounding is triggered by the budget, which is deliberately conservative —
// half the line cap. A payload can cross it and still frame comfortably. When
// nothing inside such a payload is long enough to elide, the only faithful
// answer is to relay it: deleting it buys no frame that was in danger, and a
// consumer that reads Event.Raw gets nothing where it could have had
// everything. This is the case the first implementation of the bound got
// wrong, and no other test in this file reaches it, because every other
// fixture here has an elidable string in it.
func TestAPayloadOverBudgetButUnderTheCapIsRelayedWhole(t *testing.T) {
	line := manySmallStrings(t, 420, 1000)
	ev := Event{Type: "user", SessionID: "sid-2", RecordID: "rec-2", Raw: line}

	// The fixture must actually sit in the gap, or this test proves nothing:
	// over the budget that triggers bounding, under the frame the wire takes.
	unbounded, err := json.Marshal(eventWire(ev))
	if err != nil {
		t.Fatal(err)
	}
	if len(unbounded) <= maxWirePayloadBytes {
		t.Fatalf("fixture is under the budget (%d bytes): bounding is never triggered", len(unbounded))
	}

	raw, frame := frameFor(t, ev)
	if frame > broker.MaxLineLen {
		t.Fatalf("fixture is over the frame cap (%d bytes): it is not in the gap", frame)
	}

	back, err := DecodeEventWire(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Truncated {
		t.Error("a payload the wire can carry was reported as bounded")
	}
	if !bytes.Equal(back.Raw, line) {
		t.Errorf("a payload the wire can carry was not relayed whole: %d bytes of %d", len(back.Raw), len(line))
	}
}

// TestAnUnshrinkablePayloadIsStillRelayedWithoutItsRaw is the other side of
// that boundary: past the frame cap with nothing elidable, the payload cannot
// be carried at all, so the event goes without it rather than as a frame the
// wire refuses — and says so.
func TestAnUnshrinkablePayloadIsStillRelayedWithoutItsRaw(t *testing.T) {
	ev := Event{Type: "user", SessionID: "sid-3", RecordID: "rec-3",
		Raw: manySmallStrings(t, 4000, 1000)}

	raw, frame := frameFor(t, ev)
	if frame > broker.MaxLineLen {
		t.Fatalf("frame is %d bytes, over the %d-byte cap", frame, broker.MaxLineLen)
	}
	back, err := DecodeEventWire(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !back.Truncated {
		t.Error("an event relayed without its payload must say so")
	}
	if back.SessionID != "sid-3" || back.RecordID != "rec-3" {
		t.Errorf("the event's identity was lost with its payload: %+v", back)
	}
}
