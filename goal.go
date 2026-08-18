// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
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
}

func (a *Agent) noteGoalEvent(ev Event) {
	if a.goal == "" || a.goalClosed {
		return
	}
	if ev.IsError {
		if a.goalTimer != nil {
			a.goalTimer.Stop()
			a.goalTimer = nil
		}
		a.goalSeenTerminal = false
		a.goalTurn.Reset()
		return
	}
	if ev.Type != "assistant" {
		return
	}
	if ev.Text != "" {
		if a.goalTurn.Len() > 0 {
			a.goalTurn.WriteByte('\n')
		}
		a.goalTurn.WriteString(ev.Text)
	}
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
	if status, ok := parseGoalStatus(text); ok {
		a.closeGoalLocked()
		a.mu.Unlock()
		slog.Info("claudia goal closed by status", "session", a.sessionID, "status", status)
		return
	}
	goal := a.goal
	a.goalSeenTerminal = false
	a.goalTurn.Reset()
	a.goalTimer = nil
	a.mu.Unlock()

	if a.PromptInFlight() {
		return
	}
	if err := a.Send(goalContinuation(goal)); err != nil {
		slog.Warn("claudia goal continuation failed", "session", a.sessionID, "err", err)
		a.closeGoal()
	}
}

func parseGoalStatus(text string) (string, bool) {
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

func goalContinuation(goal string) string {
	return "Continue the open objective. Ending the previous turn did not complete it.\n" +
		"Objective:\n" + goal + "\n\n" +
		"Keep working until the objective is evidenced complete or blocked.\n" +
		"When complete, emit a line exactly:\n" + GoalStatusComplete + "\n" +
		"When blocked with no remaining path, emit a line exactly:\n" + GoalStatusBlocked + "\n" +
		"Do not emit either line unless that condition holds."
}
