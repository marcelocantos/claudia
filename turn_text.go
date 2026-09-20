// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "strings"

// appendTurnText accumulates one assistant event's text into the reply a
// turn hands back (WaitForResponse, and the goal loop's view of the turn
// it must judge).
//
// Two backend shapes share this event stream and they need opposite
// treatment. Claude publishes one event per *content block* of a finished
// message: whole paragraphs that read as written only when a newline
// divides them. ACP backends (Cursor, Grok) publish streaming *token
// deltas* flagged PreviewUpdateAppend, where "p" and "ong" are the two
// halves of one word — a separator between those corrupts the reply into
// "p\nong" (🎯T79). So a delta concatenates and a block gets a newline.
func appendTurnText(b *strings.Builder, ev Event) {
	if ev.Text == "" {
		return
	}
	if b.Len() > 0 && ev.PreviewUpdate != PreviewUpdateAppend {
		b.WriteByte('\n')
	}
	b.WriteString(ev.Text)
}
