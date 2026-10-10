// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

// Request correlation is seat-local. The pending FIFO binds host sends to
// provider turn ids at the first event (including ACP prompt_accepted, which
// can arrive synchronously inside Send). A Cursor reissue publishes another
// prompt_accepted with no host send: it inherits the still-active request.
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
	if a.brokerGrant != "" || ev.TurnID == "" {
		return
	}
	if a.requestByTurn == nil {
		a.requestByTurn = make(map[string]string)
	}
	id, known := a.requestByTurn[ev.TurnID]
	if !known && len(a.requestPending) > 0 {
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
		if ev.TurnID == a.requestActiveTurn {
			clear(a.requestByTurn)
			a.requestActiveTurn = ""
		} else {
			delete(a.requestByTurn, ev.TurnID)
		}
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
