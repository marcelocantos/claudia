// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"strings"
	"testing"
)

// The mutant this pins is the one that shipped: joining every assistant
// event with "\n", which turned a streamed "p" + "ong" into "p\nong"
// (🎯T79). Claude's block-per-event shape must keep its newline.
func TestAppendTurnText(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []Event
		want string
	}{{
		name: "acp deltas concatenate",
		evs: []Event{
			{Text: "p", PreviewUpdate: PreviewUpdateAppend},
			{Text: "ong", PreviewUpdate: PreviewUpdateAppend},
		},
		want: "pong",
	}, {
		name: "claude content blocks keep their newline",
		evs:  []Event{{Text: "thinking"}, {Text: "answer"}},
		want: "thinking\nanswer",
	}, {
		name: "empty text contributes nothing, not a blank line",
		evs: []Event{
			{Text: "p", PreviewUpdate: PreviewUpdateAppend},
			{Text: ""},
			{Text: "ong", PreviewUpdate: PreviewUpdateAppend},
		},
		want: "pong",
	}, {
		name: "a delta that opens the turn does not lead with a separator",
		evs:  []Event{{Text: "pong", PreviewUpdate: PreviewUpdateAppend}},
		want: "pong",
	}, {
		name: "whitespace the agent streamed is preserved verbatim",
		evs: []Event{
			{Text: "line one\n", PreviewUpdate: PreviewUpdateAppend},
			{Text: "line two", PreviewUpdate: PreviewUpdateAppend},
		},
		want: "line one\nline two",
	}, {
		name: "a sealed block after deltas is a new block",
		evs: []Event{
			{Text: "pong", PreviewUpdate: PreviewUpdateAppend},
			{Text: "GOAL_COMPLETE"},
		},
		want: "pong\nGOAL_COMPLETE",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			for _, ev := range tc.evs {
				appendTurnText(&b, ev)
			}
			if got := b.String(); got != tc.want {
				t.Fatalf("accumulated %q, want %q", got, tc.want)
			}
		})
	}
}
