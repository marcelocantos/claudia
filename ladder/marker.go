// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"strings"
)

// The reserved delimiter pair for layer-to-layer signalling.
//
// U+27E6/U+27E7 rather than square brackets, and the choice is what
// makes the channel enforceable. Brackets collide with prose, markdown
// links and code, so asking every stage of a pipeline to preserve them
// is unenforceable by inspection. A reserved pair makes it a counting
// test: count them in, count them out, and any stage that eats one
// fails.
//
// The resonance is apt — in denotational semantics ⟦x⟧ already denotes
// "the meaning of x", which is exactly what a resolution annotation
// asserts.
const (
	MarkerOpen  = '⟦' // ⟦
	MarkerClose = '⟧' // ⟧
)

// MarkerKind classifies what a marker is saying. The channel is general
// rather than ambiguity-specific: provenance, staleness and elision all
// want the same mechanism, and generalising now costs nothing while
// retrofitting later would cost a migration.
type MarkerKind string

const (
	// MarkerResolution is a provisional resolution: ⟦resolution:your
	// world → yourworld2?⟧.
	//
	// This is the insight that dissolves the resolve-or-escalate
	// binary. A layer that is unsure need not choose between guessing
	// silently and paying for an escalation: it resolves provisionally
	// and MARKS it. Escalation is binary and costs a model turn; an
	// annotation is continuous and costs about a dozen tokens.
	//
	// A marked guess is an artifact. An unmarked guess leaves nothing
	// to demote on.
	MarkerResolution MarkerKind = "resolution"

	// MarkerProvenance records which rung produced something, so "a
	// rule said X" and "the model said X" stay distinguishable.
	MarkerProvenance MarkerKind = "provenance"

	// MarkerStaleness records how old the state behind a claim is.
	MarkerStaleness MarkerKind = "staleness"

	// MarkerElision records what was left out and how to fetch it. This
	// is how passing bulk content by reference is expressed on the
	// wire.
	MarkerElision MarkerKind = "elision"

	// MarkerAttention flags the anomalous item, so a higher rung does
	// not have to rediscover it.
	MarkerAttention MarkerKind = "attention"
)

// Marker is one out-of-band signal.
type Marker struct {
	Kind MarkerKind
	Text string
}

// String renders the marker for the wire.
//
// Rare codepoints tokenize to several tokens each, so a marked
// resolution costs roughly eight tokens rather than two: negligible when
// sparse, and an argument against marking liberally. Gating on
// confidence is the consumer's to do.
func (m Marker) String() string {
	return string(MarkerOpen) + string(m.Kind) + ":" + m.Text + string(MarkerClose)
}

// CountMarkers returns how many well-formed markers a string carries.
// This is the counting test that makes survival mechanically checkable.
func CountMarkers(s string) int {
	return len(ParseMarkers(s))
}

// ParseMarkers extracts every well-formed marker, in order. A marker
// with no recognised kind parses with an empty Kind rather than being
// dropped, because silently discarding an unknown signal is the same
// failure the channel exists to prevent.
func ParseMarkers(s string) []Marker {
	var out []Marker
	for {
		i := strings.IndexRune(s, MarkerOpen)
		if i < 0 {
			return out
		}
		rest := s[i+len(string(MarkerOpen)):]
		j := strings.IndexRune(rest, MarkerClose)
		if j < 0 {
			return out
		}
		body := rest[:j]
		s = rest[j+len(string(MarkerClose)):]

		m := Marker{}
		if kind, text, ok := strings.Cut(body, ":"); ok {
			m.Kind, m.Text = MarkerKind(kind), text
		} else {
			m.Text = body
		}
		out = append(out, m)
	}
}

// Sanitized is untrusted text with its markers removed, plus what was
// removed.
type Sanitized struct {
	// Text is safe to pass on.
	Text string
	// Stripped is what was taken out. It is RETURNED rather than
	// discarded: a marker arriving from an untrusted source is a
	// forgery attempt, and it should reach an auditor as an event
	// rather than vanish.
	Stripped []Marker
}

// Forged reports whether the input tried to signal on the channel.
func (s Sanitized) Forged() bool { return len(s.Stripped) > 0 }

// Sanitize strips markers from untrusted input.
//
// The channel is a TRUST BOUNDARY. Anything able to emit reserved
// codepoints can signal to layers that act on them, so text arriving
// from outside — a worker's report, a model's output relayed onward, a
// user's message — is control-channel injection if it carries them.
// Markers are stripped on the way in, and the right to emit them is a
// manifest question rather than a convention.
func Sanitize(untrusted string) Sanitized {
	markers := ParseMarkers(untrusted)
	if len(markers) == 0 {
		// Unbalanced delimiters are still not allowed through: a lone
		// opener could swallow a later legitimate marker's close.
		return Sanitized{Text: removeDelimiters(untrusted)}
	}

	var b strings.Builder
	s := untrusted
	for {
		i := strings.IndexRune(s, MarkerOpen)
		if i < 0 {
			b.WriteString(removeDelimiters(s))
			break
		}
		rest := s[i+len(string(MarkerOpen)):]
		j := strings.IndexRune(rest, MarkerClose)
		if j < 0 {
			b.WriteString(removeDelimiters(s))
			break
		}
		b.WriteString(s[:i])
		s = rest[j+len(string(MarkerClose)):]
	}
	return Sanitized{Text: b.String(), Stripped: markers}
}

func removeDelimiters(s string) string {
	return strings.Map(func(r rune) rune {
		if r == MarkerOpen || r == MarkerClose {
			return -1
		}
		return r
	}, s)
}

// Annotate appends a marker to text, which is what a layer does when it
// resolves something provisionally rather than escalating.
func Annotate(text string, m Marker) string {
	if text == "" {
		return m.String()
	}
	return text + " " + m.String()
}

// NoteFromMarker converts a marker into a request note, so an
// annotation crossing into the in-process world keeps its meaning.
func NoteFromMarker(layer string, m Marker) Note {
	return Note{Layer: layer, Kind: string(m.Kind), Text: m.Text}
}
