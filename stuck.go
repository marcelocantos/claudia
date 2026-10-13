// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"strings"
	"unicode/utf8"
)

const (
	// ProgressModelSwitch is the ProgressType on the Event a seat publishes
	// when it moves to another provider or model in place (🎯T195).
	ProgressModelSwitch = "model_switch"
	// ProgressStuck is the ProgressType on a Type=system Event when a
	// turn ends in a classified rate_limit or quota wall. Claudia does
	// not itself switch the seat in response.
	ProgressStuck = "stuck"

	StuckClassRateLimit = "rate_limit"
	StuckClassQuota     = "quota"
)

func clipRunes(s string, max int) string {
	if max < 1 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func classifyStuckEvent(ev Event) (class, detail string) {
	if !ev.IsError {
		return "", ""
	}
	blob := strings.ToLower(ev.Text + " " + ev.StopReason + " " + string(ev.Raw))
	if strings.Contains(blob, "client_bug") || strings.Contains(blob, "model_not_found") {
		return "", ""
	}
	if strings.Contains(blob, "unauthorized") || strings.Contains(blob, "auth") && !strings.Contains(blob, "rate") {
		if !strings.Contains(blob, "rate_limit") && !strings.Contains(blob, "rate limit") {
			return "", ""
		}
	}
	detail = clipRunes(strings.TrimSpace(ev.Text), 240)
	if strings.Contains(blob, "rate_limit") || strings.Contains(blob, "rate limit") || strings.Contains(blob, "429") {
		return StuckClassRateLimit, detail
	}
	if strings.Contains(blob, "quota") || strings.Contains(blob, "spend") ||
		strings.Contains(blob, "usage limit") || strings.Contains(blob, "credits") {
		return StuckClassQuota, detail
	}
	return "", ""
}
