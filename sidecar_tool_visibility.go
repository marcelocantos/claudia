// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "strings"

// NormalizeSidecarToolEndStatus maps a sidecar tool_end status to the host
// ToolStatus contract (jevons 🎯T1218). Unknown wire values are not success.
func NormalizeSidecarToolEndStatus(sidecar string) string {
	switch strings.TrimSpace(sidecar) {
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	case "denied":
		return "denied"
	case "unknown":
		return "unknown"
	default:
		return "unknown"
	}
}

// PublishSidecarToolStart is the typed begin event for native and setTools
// tools. Raw is intentionally unset so Je updates by ToolCallID.
func PublishSidecarToolStart(callID, name, text string) Event {
	return Event{
		Type:         "progress",
		ProgressType: ProgressToolUse,
		ToolCallID:   callID,
		ToolTitle:    name,
		Text:         text,
		ToolStatus:   "in_progress",
	}
}

// PublishSidecarToolEnd is the typed terminal event for the same call id.
func PublishSidecarToolEnd(callID, name, text, sidecarStatus string) Event {
	return Event{
		Type:         "progress",
		ProgressType: ProgressToolUse,
		ToolCallID:   callID,
		ToolTitle:    name,
		Text:         text,
		ToolStatus:   NormalizeSidecarToolEndStatus(sidecarStatus),
	}
}
