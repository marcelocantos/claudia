// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import "strings"

// Causes a seat turn can name (🎯T870). The sidecar records the same set.
const (
	CauseOwner        = "owner"
	CauseRestartNudge = "restart-nudge"
	CauseSentinel     = "sentinel"
	CauseImpatience   = "impatience"
	CauseAgentForward = "agent-forward"
	CauseRSI          = "rsi"
	CauseCapacity     = "capacity"
	CauseSteer        = "steer"
)

// OneLine is the cause detail: the first line of the prompt, capped,
// not the transcript.
func OneLine(text string) string {
	s := strings.TrimSpace(text)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 160
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// ValidCause reports whether cause is one of the turn-record causes.
func ValidCause(cause string) bool {
	switch cause {
	case CauseOwner, CauseRestartNudge, CauseSentinel, CauseImpatience,
		CauseAgentForward, CauseRSI, CauseCapacity, CauseSteer:
		return true
	default:
		return false
	}
}

// ValidResume reports whether resume is a ResumeAll how-value.
func ValidResume(resume string) bool {
	switch resume {
	case "adopted", "launched", "reminted":
		return true
	default:
		return false
	}
}

// ClassifyCause names who prompted a seat from the prompt text.
// Steer is not inferred from prose; the steer verb sets it.
func ClassifyCause(text string) (cause, detail string) {
	detail = OneLine(text)
	switch {
	case strings.Contains(text, "The host restarted at"),
		strings.Contains(text, "[claudia] The host restarted"):
		return CauseRestartNudge, detail
	case strings.Contains(text, "[event: sentinel]"),
		strings.Contains(text, "[event:sentinel]"):
		return CauseSentinel, detail
	case strings.Contains(strings.ToLower(text), "impatience incident"):
		return CauseImpatience, detail
	case strings.Contains(text, "[Agent ") && strings.Contains(text, " responded]"):
		return CauseAgentForward, detail
	case strings.Contains(text, "[event: rsi-coach]"),
		strings.Contains(text, "[event: rsi]"):
		return CauseRSI, detail
	case strings.Contains(text, "[event: capacity]"):
		return CauseCapacity, detail
	default:
		return CauseOwner, detail
	}
}
