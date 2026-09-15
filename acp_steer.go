// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"strconv"
)

// ErrTurnInFlight is the typed busy error a plain Prompt returns while a
// turn is already running on an ACP session (🎯T72.1). The host decides
// what to do with the text — queue it, steer, or interrupt — so the client
// never auto-steers. The message keeps the historical "prompt already in
// flight" wording that jevons classifies as busy (agenterr.IsPromptBusy).
var ErrTurnInFlight = errors.New("prompt already in flight")

// acpSteerMechanism names how ACP steer is delivered: a second
// session/prompt written while the first is still running, whose id
// supersedes the earlier one for settling the turn. It is the
// DeliveryOutcome.Mechanism value for Cursor and Grok steer.
const acpSteerMechanism = "acp_session_prompt_supersede"

// acpSubmitMechanism names a plain session/prompt on an idle session —
// what Steer degrades to when there is no turn to fold into.
const acpSubmitMechanism = "acp_session_prompt"

// acpPromptSettle is how one JSON-RPC result affects the seat's turn.
type acpPromptSettle int

const (
	// acpPromptNotOurs: the id is not a session/prompt of the open turn.
	acpPromptNotOurs acpPromptSettle = iota
	// acpPromptSuperseded: an earlier, steered-over prompt returned. The
	// turn continues on the top id; no terminal event.
	acpPromptSuperseded
	// acpPromptTurnDone: the top id returned. The turn is over.
	acpPromptTurnDone
)

// acpPromptStack tracks the session/prompt request ids that share one
// seat turn. A plain prompt pushes the first id; each steer pushes another
// on top. The top id settles the turn (🎯T72.1): a result for any lower id
// is a superseded prompt whose early return must not end the turn.
//
// Callers hold the owning client's mutex.
type acpPromptStack struct {
	ids []int64
	// lastSuperseded is the id the most recent steer pushed over, kept as
	// DeliveryOutcome.SupersededTurnID. Zero until the first steer.
	lastSuperseded int64
}

// top returns the id that settles the turn, or 0 when idle.
func (s *acpPromptStack) top() int64 {
	if len(s.ids) == 0 {
		return 0
	}
	return s.ids[len(s.ids)-1]
}

// inFlight reports whether any prompt of the open turn is unanswered.
func (s *acpPromptStack) inFlight() bool {
	return len(s.ids) > 0
}

// push opens a turn (empty stack) or steers the open one (non-empty).
func (s *acpPromptStack) push(id int64) {
	if prev := s.top(); prev != 0 {
		s.lastSuperseded = prev
	}
	s.ids = append(s.ids, id)
}

// clear forgets every id: the turn was cancelled or settled.
func (s *acpPromptStack) clear() {
	s.ids = nil
}

// settle removes id from the stack and reports what its result means.
// A top-id result clears the whole stack: any superseded prompt that has
// not answered yet belongs to a turn that is over, and its late result is
// then acpPromptNotOurs.
func (s *acpPromptStack) settle(id int64) acpPromptSettle {
	if id == 0 || len(s.ids) == 0 {
		return acpPromptNotOurs
	}
	if s.top() == id {
		s.clear()
		return acpPromptTurnDone
	}
	for i, v := range s.ids {
		if v == id {
			s.ids = append(s.ids[:i], s.ids[i+1:]...)
			return acpPromptSuperseded
		}
	}
	return acpPromptNotOurs
}

// supersededTurnID formats lastSuperseded for DeliveryOutcome, "" if none.
func (s *acpPromptStack) supersededTurnID() string {
	if s.lastSuperseded == 0 {
		return ""
	}
	return strconv.FormatInt(s.lastSuperseded, 10)
}

// acpPromptRequest is the session/prompt JSON-RPC request both ACP
// clients write; a steer is the same message with a fresh id.
func acpPromptRequest(id int64, sessionID, text string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "session/prompt",
		"params": map[string]any{
			"sessionId": sessionID,
			"prompt": []map[string]any{
				{"type": "text", "text": text},
			},
		},
	}
}
