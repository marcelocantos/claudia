// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

// Request correlation is seat-local. A pending host send binds to a provider
// turn only when that turn is accepted: prompt_accepted (ACP, or the Codex
// turn/start result), or a Claude user-prompt transcript line. An event whose
// turn id this process never accepted does not consume the next pending send,
// even when it is published inside that Send. A Cursor reissue publishes
// another prompt_accepted with no host send and inherits the still-active
// request. A turn id that already closed stays on its original request.
// Provider terminal normalization does not change this mapping.
func (a *Agent) queueRequestID(id string) {
	a.mu.Lock()
	a.requestPending = append(a.requestPending, id) // empty is a real, uncorrelated send
	a.mu.Unlock()
}

func (a *Agent) discardPendingRequestID(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, pending := range a.requestPending {
		if pending == id {
			a.requestPending = append(a.requestPending[:i], a.requestPending[i+1:]...)
			return
		}
	}
}

func (a *Agent) correlateRequestLocked(ev *Event) {
	// Sidecar receipts carry identity on the wire. A queued input may be
	// absorbed by an existing turn, so no single FIFO/turn binding is valid.
	// The journal is the restart record. Events after a reconnect name the
	// request themselves. This map only fills a turn-scoped event that
	// arrived without one, from an earlier event for that same turn.
	if a.ops.explicitRequestIDs {
		a.correlateExplicitLocked(ev)
		return
	}
	if ev.RequestID != "" {
		return
	}
	if a.brokerGrant != "" || ev.TurnID == "" {
		return
	}
	// A predecessor turn already closed. Stamp its request and leave the
	// pending host send for a turn this process has not seen.
	if id, ok := a.requestRetired[ev.TurnID]; ok {
		ev.RequestID = id
		return
	}
	if a.requestByTurn == nil {
		a.requestByTurn = make(map[string]string)
	}
	id, known := a.requestByTurn[ev.TurnID]
	if !known && len(a.requestPending) > 0 && eventAcceptsRequest(ev) {
		id = a.requestPending[0]
		a.requestPending = a.requestPending[1:]
		a.requestByTurn[ev.TurnID] = id
		a.requestActiveTurn = ev.TurnID
	} else if !known && a.provider == ProviderCursor && ev.ProgressType == ProgressPromptAccepted && a.requestActiveTurn != "" {
		// Reissued Cursor delivery. The old provider id remains redeemable
		// and the surviving id must resolve to the same logical request.
		id = a.requestByTurn[a.requestActiveTurn]
		a.requestByTurn[ev.TurnID] = id
		a.requestActiveTurn = ev.TurnID
	}
	ev.RequestID = id
	// Final is per OMP attempt, not necessarily per logical request.
	// All supported paths publish an assistant terminal stop for the actual
	// logical close (or a terminal error without an assistant answer).
	if ev.IsTerminalStop() || ev.IsError {
		a.retireRequestTurnLocked(ev.TurnID)
	}
}

// correlateExplicitLocked keeps one host request for each provider turn.
// A later acceptance on that turn, such as a steer, is the request that
// following events inherit. A turn that already closed keeps its last
// request. Nothing is taken from requestPending.
func (a *Agent) correlateExplicitLocked(ev *Event) {
	if ev.TurnID == "" {
		return
	}
	if a.requestByTurn == nil {
		a.requestByTurn = make(map[string]string)
	}
	if ev.RequestID != "" {
		if id, retired := a.requestRetired[ev.TurnID]; retired {
			ev.RequestID = id
		} else {
			a.requestByTurn[ev.TurnID] = ev.RequestID
			a.requestActiveTurn = ev.TurnID
		}
	} else if id, ok := a.requestRetired[ev.TurnID]; ok {
		ev.RequestID = id
	} else if id, ok := a.requestByTurn[ev.TurnID]; ok {
		ev.RequestID = id
	}
	if ev.RequestID != "" && (ev.IsTerminalStop() || ev.IsError) {
		a.retireRequestTurnLocked(ev.TurnID)
	}
}

// eventAcceptsRequest reports whether ev is the provider taking a queued
// host send onto this turn. A later assistant, tool, or error line is not
// acceptance, and neither is a Claude tool_result recorded with role user.
func eventAcceptsRequest(ev *Event) bool {
	if ev.ProgressType == ProgressPromptAccepted {
		return true
	}
	return isUserPromptLine(ev.Raw)
}

// retireRequestTurnLocked remembers a closed turn's host request. The active
// turn's close also retires every provider id bound to it, including a Cursor
// reissue, then drops the live map. A later event on any of those ids must
// not bind the next queued send.
func (a *Agent) retireRequestTurnLocked(turnID string) {
	if a.requestRetired == nil {
		a.requestRetired = make(map[string]string)
	}
	if turnID == a.requestActiveTurn {
		for turn, id := range a.requestByTurn {
			a.requestRetired[turn] = id
		}
		clear(a.requestByTurn)
		a.requestActiveTurn = ""
		return
	}
	if id, ok := a.requestByTurn[turnID]; ok {
		a.requestRetired[turnID] = id
		delete(a.requestByTurn, turnID)
	}
}

// Codex turn/steer adds input to the existing turn (no new provider id or
// prompt_accepted event). Once the steer RPC succeeds, transfer the pending
// host identity onto that turn; ACP steers instead bind at prompt_accepted.
func (a *Agent) bindSameTurnSteer(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, pending := range a.requestPending {
		if pending != id {
			continue
		}
		a.requestPending = append(a.requestPending[:i], a.requestPending[i+1:]...)
		if a.requestActiveTurn != "" {
			a.requestByTurn[a.requestActiveTurn] = id
		}
		return
	}
}
