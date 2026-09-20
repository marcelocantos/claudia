// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
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
	// acpPromptRedeemed: a delivery the silence re-issue abandoned came
	// back with the turn's answer after all. The turn is over, but the
	// terminal event belongs to the surviving delivery's id, because that
	// is the id the turn's chunks were published under.
	acpPromptRedeemed
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
	// abandoned holds ids the 🎯T83 silence re-issue gave up on. They are
	// not steered-over prompts: a steer is new work that supersedes old
	// work, while a re-issue is the SAME text delivered twice because the
	// peer said nothing the first time. Only one answer is coming, and it
	// may well come back under the delivery this client stopped waiting
	// for — a peer that was merely slow answers what it was asked.
	//
	// Dropping those ids outright is what 🎯T92 was filed for. Their
	// results then settled as acpPromptNotOurs and published nothing at
	// all, so a caller in WaitForResponse had no terminal event left to
	// wait for: the reply was on the wire and the turn never ended.
	abandoned map[int64]bool
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

// clear forgets every id: the turn was cancelled or settled. Abandoned
// deliveries go with it — once the turn is over, a straggler belongs to
// nobody and must not redeem itself into whatever comes next.
func (s *acpPromptStack) clear() {
	s.ids = nil
	s.abandoned = nil
}

// reissue replaces the open turn's deliveries with a single fresh one
// after the silence watch gave up, keeping the old ids redeemable
// (🎯T92). It is one critical section with the caller's decision to
// abandon, so a reply racing the re-delivery is either seen before the
// swap — nothing to re-establish — or arrives to find its id still known.
func (s *acpPromptStack) reissue(id int64) {
	for _, old := range s.ids {
		if s.abandoned == nil {
			s.abandoned = make(map[int64]bool, 1)
		}
		s.abandoned[old] = true
	}
	s.ids = []int64{id}
}

// settle removes id from the stack and reports what its result means.
// A top-id result clears the whole stack: any superseded prompt that has
// not answered yet belongs to a turn that is over, and its late result is
// then acpPromptNotOurs.
//
// answered says the result carries the peer's answer rather than its
// acknowledgement of a cancellation. It decides only the abandoned case:
// the re-issue path writes session/cancel before re-delivering, so the
// abandoned id comes back either as the reply this client nearly lost or
// as the cancel landing. Reading the second as the first would end the
// turn with nothing in it — `reply ""` by a different road (🎯T92).
func (s *acpPromptStack) settle(id int64, answered bool) acpPromptSettle {
	if id == 0 {
		return acpPromptNotOurs
	}
	if len(s.ids) > 0 && s.top() == id {
		s.clear()
		return acpPromptTurnDone
	}
	if s.abandoned[id] {
		delete(s.abandoned, id)
		if !answered || len(s.ids) == 0 {
			// The cancel landed (or the turn is already over). The
			// surviving delivery still owes the caller a terminal event.
			return acpPromptSuperseded
		}
		s.clear()
		return acpPromptRedeemed
	}
	for i, v := range s.ids {
		if v == id {
			s.ids = append(s.ids[:i], s.ids[i+1:]...)
			return acpPromptSuperseded
		}
	}
	return acpPromptNotOurs
}

// acpResultAnswersTurn reports whether a session/prompt result carries
// the peer's answer to the prompt rather than its acknowledgement that
// the prompt was cancelled. A JSON-RPC error is not an answer either:
// on an abandoned delivery the likeliest thing behind one is the
// re-issue path's own session/cancel.
func acpResultAnswersTurn(msg acpRPCMessage) bool {
	if msg.Error != nil {
		return false
	}
	var meta struct {
		StopReason string `json:"stopReason"`
	}
	if len(msg.Result) > 0 {
		_ = json.Unmarshal(msg.Result, &meta)
	}
	return meta.StopReason != "cancelled"
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
