// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marcelocantos/claudia/internal/broker"
)

// Bounding provider payloads for the broker wire (🎯T73).
//
// The broker multiplexes every response and push for one consumer onto one
// connection, and a line over broker.MaxLineLen cannot be relayed. The
// provider side is far more permissive — acpMaxJSONLine allows 16 MiB — so a
// screenshot tool_result is a legal provider line and an impossible broker
// frame. The jevons 🎯T661 incident is what that costs when nothing bounds it:
// a 1.2 KB send died because an unrelated PNG line closed the connection they
// were sharing.
//
// So the payload is bounded here, at the codec every relayed event passes
// through, rather than discovered at the socket. Two rules decide the shape:
//
//   - The result still parses. Consumers read Event.Raw as JSON, so an
//     oversized payload is shrunk by replacing the strings inside it, never by
//     cutting the bytes at an offset. A half message that happens to parse is
//     the failure this avoids, not the one it creates.
//   - The result says so. Every elision leaves a marker naming the bytes it
//     replaced, and the event carries Truncated, so a consumer can tell a
//     bounded payload from a complete one instead of inferring it.

const (
	// maxWirePayloadBytes is the budget for one encoded event payload. It is
	// well under broker.MaxLineLen because the payload is not the frame: the
	// response envelope wraps it, and Event.Raw is []byte, which JSON renders
	// as base64 and a third larger again. The slack is what keeps a payload
	// that fits the budget from being a frame that does not.
	maxWirePayloadBytes = broker.MaxLineLen / 2

	// maxWireFrameBytes is the hard ceiling, as distinct from the budget: the
	// largest encoded payload that still leaves the response envelope room
	// inside one broker frame. maxWirePayloadBytes is what bounding aims for
	// and is deliberately below this, so a payload landing between the two is
	// relayed whole — discarding one the wire would have carried loses data
	// for nothing, and the budget exists to leave slack, not to forbid.
	maxWireFrameBytes = broker.MaxLineLen - wireEnvelopeHeadroom

	// wireEnvelopeHeadroom is what the agent_event response wraps around the
	// payload: the type, the grant name, and the JSON around them. It is a
	// round over-estimate on purpose. Being wrong high costs a payload
	// relayed at the budget rather than the ceiling; being wrong low costs a
	// frame the wire refuses, which is the whole defect.
	wireEnvelopeHeadroom = 4 << 10

	// maxWireStringBytes is the longest single string kept inside a bounded
	// payload. A few hundred bytes of assistant text, a tool name, a file
	// path: all far below it. A base64 image: far above.
	maxWireStringBytes = 16 << 10
)

// elidedMarker is what replaces a string too long to relay. It names the size
// so a consumer reading the payload sees what it is missing rather than a
// truncated value that looks like data.
func elidedMarker(n int) string {
	return fmt.Sprintf("[claudia: elided %d bytes, too large for the broker wire]", n)
}

// BoundWirePayload shrinks one provider JSONL line to fit a broker frame,
// returning the bounded line and whether anything was elided.
//
// It replaces every string longer than maxWireStringBytes — which is where a
// base64 image lives — with a marker naming its size, and re-encodes. The
// result is still the same JSON document with the same keys, so a consumer
// parses it exactly as before.
//
// A line already within budget is returned unchanged, byte for byte: the
// common case must not be re-encoded, both because it is wasteful and because
// re-encoding is the one thing that could alter a payload nobody asked to
// change. A line over budget that is not valid JSON at all cannot be shrunk
// safely and is reported as elided with no content, since relaying a fragment
// of it would be worse than relaying none.
func BoundWirePayload(line []byte) ([]byte, bool) {
	if len(line) <= maxWirePayloadBytes {
		return line, false
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	// Numbers stay as written. Decoding into any would turn every integer
	// into a float64 and re-encode a turn id or a token count as 1.234e+06.
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return json.RawMessage(fmt.Sprintf("%q", elidedMarker(len(line)))), true
	}
	bounded, _ := elideLongStrings(doc, maxWireStringBytes)
	out, err := json.Marshal(bounded)
	if err != nil || len(out) > maxWirePayloadBytes {
		// Nothing single is oversized, so the size is in the shape: thousands
		// of small strings, or an array too long to relay. There is no
		// faithful smaller version, so the payload becomes the marker alone.
		return json.RawMessage(fmt.Sprintf("%q", elidedMarker(len(line)))), true
	}
	return out, true
}

// elideLongStrings walks a decoded JSON value and replaces every string longer
// than max with elidedMarker. It reports whether it changed anything, so a
// caller can tell a payload that was merely large from one that was bounded.
func elideLongStrings(v any, max int) (any, bool) {
	switch t := v.(type) {
	case string:
		if len(t) > max {
			return elidedMarker(len(t)), true
		}
	case map[string]any:
		changed := false
		for k, e := range t {
			if ne, c := elideLongStrings(e, max); c {
				t[k], changed = ne, true
			}
		}
		return t, changed
	case []any:
		changed := false
		for i, e := range t {
			if ne, c := elideLongStrings(e, max); c {
				t[i], changed = ne, true
			}
		}
		return t, changed
	}
	return v, false
}

// elideString bounds one plain (non-JSON) string field the same way.
func elideString(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	return elidedMarker(len(s)), true
}

// boundEventForWire returns ev with every string too long to relay replaced by
// a marker, and Truncated set when anything was elided. It reports what it
// could shrink, not what it wishes were smaller: a payload holding nothing
// long enough to elide comes back unchanged and unflagged, and it is the
// caller that decides whether the result still fits. ev is passed by value;
// the caller's Event — which is also the one a direct consumer sees — is
// never touched.
func boundEventForWire(ev Event) Event {
	raw, rawCut := BoundWirePayload(ev.Raw)
	text, textCut := elideString(ev.Text, maxWireStringBytes)
	ev.Raw, ev.Text = raw, text
	ev.Truncated = ev.Truncated || rawCut || textCut
	return ev
}

// boundTaskEventForWire is boundEventForWire for a TaskEvent. ToolInput is the
// JSON-encoded tool input, so a screenshot arrives there; Content and ErrorMsg
// are plain text and are bounded as strings.
func boundTaskEventForWire(ev TaskEvent) TaskEvent {
	if input, cut := BoundWirePayload([]byte(ev.ToolInput)); cut {
		ev.ToolInput, ev.Truncated = string(input), true
	}
	if content, cut := elideString(ev.Content, maxWireStringBytes); cut {
		ev.Content, ev.Truncated = content, true
	}
	if msg, cut := elideString(ev.ErrorMsg, maxWireStringBytes); cut {
		ev.ErrorMsg, ev.Truncated = msg, true
	}
	return ev
}

// IsWireElided reports whether s is a marker this package left in place of a
// payload too large to relay. It is what a consumer checks to tell an elided
// field from one the provider really sent, without matching on message text.
func IsWireElided(s string) bool {
	return strings.HasPrefix(s, "[claudia: elided ") && strings.HasSuffix(s, "the broker wire]")
}
