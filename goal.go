// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Host-owned goal completion markers. The Session loop looks for these
// as whole lines in assistant text. They are not forwarded to any
// provider /goal command (🎯T39).
const (
	GoalStatusComplete = "GOAL_STATUS: complete"
	GoalStatusBlocked  = "GOAL_STATUS: blocked"
)

// goalStallLimit is how many consecutive terminal turns may end without
// a single tool call before the host stops continuing the Goal and says
// so (🎯T110).
//
// A turn that calls no tool and emits neither GOAL_STATUS line has
// answered; it has not worked. One is ordinary — a seat may open by
// stating a plan. A run of them is a seat that is never going to start:
// on 2026-09-21 cl-t109-codex-git-write refused its brief and then nine
// continuations, one every 40s or so, and would have gone on refusing
// for as long as it was asked. Three is the brief and two continuations:
// enough that one odd turn does not end a Goal, few enough that a stuck
// seat costs two turns rather than a night of them.
const goalStallLimit = 3

// ProgressGoalStalled is the ProgressType on the Type=system Event
// published when the host stops continuing a Goal at goalStallLimit.
// Claudia closes the Goal and does nothing else; the host decides.
const ProgressGoalStalled = "goal_stalled"

// goalEventShowsWork reports whether ev is evidence of a tool call, in
// any backend's shape: Claude's tool_use stop reason, or the ACP and
// Codex progress events.
func goalEventShowsWork(ev Event) bool {
	if ev.Type == "assistant" && ev.StopReason == "tool_use" {
		return true
	}
	return ev.ProgressType == ProgressToolUse || ev.ToolCallID != "" || ev.ToolTitle != ""
}

// Goal reports the durable objective this Session was started with.
// Empty means one-shot Send (no host continuation).
func (a *Agent) Goal() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.goal
}

// GoalActive reports whether the host will issue a continuation after
// the next terminal assistant turn.
func (a *Agent) GoalActive() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.goal != "" && !a.goalClosed
}

// CloseGoal stops host Goal continuation without waiting for
// GOAL_STATUS. Hosts that learn mission completeness from an external
// ledger (e.g. jevons 🎯T528) call this so remint cannot reopen Continue.
func (a *Agent) CloseGoal() {
	a.closeGoal()
	if a.ops.closeGoal != nil {
		a.ops.closeGoal(a)
	}
}

// SetGoalCompleteCheck installs (or clears) the host completeness hook
// consulted before a Goal continuation Send. Safe to call after Start /
// Launch when the registry path cannot carry a function on AgentDef.
func (a *Agent) SetGoalCompleteCheck(fn func(goal, turnText string) bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.goalCompleteCheck = fn
}

func (a *Agent) closeGoal() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closeGoalLocked()
}

func (a *Agent) closeGoalLocked() {
	a.goalClosed = true
	if a.goalTimer != nil {
		a.goalTimer.Stop()
		a.goalTimer = nil
	}
	a.goalSeenTerminal = false
	a.goalTurn.Reset()
	a.goalTurnWorked = false
	a.goalIdleTurns = 0
}

func (a *Agent) noteGoalEvent(ev Event) {
	if a.goal == "" || a.goalClosed {
		return
	}
	if a.brokerGrant != "" {
		// The daemon's Agent runs the continuation loop; its Sends arrive
		// on the stream. Running it here too would double every turn.
		return
	}
	if ev.IsError {
		if a.goalTimer != nil {
			a.goalTimer.Stop()
			a.goalTimer = nil
		}
		a.goalSeenTerminal = false
		a.goalTurn.Reset()
		a.goalTurnWorked = false
		return
	}
	if goalEventShowsWork(ev) {
		a.goalTurnWorked = true
	}
	if ev.Type != "assistant" {
		return
	}
	appendTurnText(&a.goalTurn, ev)
	if ev.IsTerminalStop() {
		a.goalSeenTerminal = true
	}
	if !a.goalSeenTerminal {
		return
	}
	if a.goalTimer != nil {
		a.goalTimer.Stop()
	}
	a.goalTimer = time.AfterFunc(waitSettleDuration, a.maybeContinueGoal)
}

func (a *Agent) maybeContinueGoal() {
	a.mu.Lock()
	if a.goal == "" || a.goalClosed || !a.alive {
		a.mu.Unlock()
		return
	}
	text := a.goalTurn.String()
	if status, ok := ParseGoalStatus(text); ok {
		a.closeGoalLocked()
		a.mu.Unlock()
		slog.Info("claudia goal closed by status", "session", a.sessionID, "status", status)
		return
	}
	goal := a.goal
	check := a.goalCompleteCheck
	a.mu.Unlock()

	// Host ledger / external completeness (jevons 🎯T528): close without
	// injecting Continue when the check says the objective is done.
	if check != nil && check(goal, text) {
		a.closeGoal()
		slog.Info("claudia goal closed by host check", "session", a.sessionID)
		return
	}

	a.mu.Lock()
	if a.goal == "" || a.goalClosed || !a.alive {
		a.mu.Unlock()
		return
	}
	if a.goalTurnWorked {
		a.goalIdleTurns = 0
	} else {
		a.goalIdleTurns++
	}
	if a.goalIdleTurns >= goalStallLimit {
		idle := a.goalIdleTurns
		a.closeGoalLocked()
		a.mu.Unlock()
		detail := fmt.Sprintf("goal continuation stopped: %d consecutive turns ended without a tool call or a GOAL_STATUS line", idle)
		slog.Warn("claudia goal stalled", "session", a.sessionID, "idle_turns", idle)
		a.publishEvent(Event{
			Type:         "system",
			ProgressType: ProgressGoalStalled,
			SessionID:    a.SessionID(),
			Text:         detail,
		})
		return
	}
	a.goalSeenTerminal = false
	a.goalTurn.Reset()
	a.goalTurnWorked = false
	a.goalTimer = nil
	a.mu.Unlock()

	if a.PromptInFlight() {
		return
	}
	msg := goalContinuation(goal)
	slog.Info("claudia goal continuation", "session", a.sessionID, "bytes", len(msg))
	if err := a.Send(msg); err != nil {
		slog.Warn("claudia goal continuation failed", "session", a.sessionID, "err", err)
		a.closeGoal()
	}
}

// ParseGoalStatus reports a whole-line GOAL_STATUS: complete / blocked
// marker in assistant text.
func ParseGoalStatus(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		switch strings.TrimSpace(line) {
		case GoalStatusComplete:
			return GoalStatusComplete, true
		case GoalStatusBlocked:
			return GoalStatusBlocked, true
		}
	}
	return "", false
}

// goalContinuationMarker opens every host-authored continuation prompt.
// It is the one string an observer can match to say "this turn is the
// host continuing the goal, not the user speaking" — the live journey's
// oracle matches it, so it is named here rather than written twice.
const goalContinuationMarker = "Continue the open objective"

func goalContinuation(goal string) string {
	// One line under the Claude Session paste threshold (400 bytes in
	// internal/tmuxagent). Newlines or len>=400 take the paste-chip
	// branch; right after a turn that path flakes on submit confirmation
	// (live 🎯T39 journey). Keep the boilerplate short so typical Goal
	// strings stay on send-keys -l.
	obj := strings.Join(strings.Fields(strings.TrimSpace(goal)), " ")
	return goalContinuationMarker + " (previous turn did not finish it). " +
		"Objective: " + obj + ". " +
		"Work until evidenced complete or blocked. " +
		"When complete emit exactly: " + GoalStatusComplete + " " +
		"When blocked emit exactly: " + GoalStatusBlocked + " " +
		"Emit neither unless true."
}
