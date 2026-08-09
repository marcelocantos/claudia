// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package fakewire holds the wire shapes the fake `claude` double emits, and
// the pure rendering of one scenario into that wire form.
//
// It is a leaf package on purpose. The helper in brokertest imports `testing`
// and must not be linked into the fake binary, while the fake binary and the
// helper must agree byte-for-byte on what a 429, a usage block and a readiness
// frame look like — that agreement is the whole point of the double. Keeping
// the shapes here means there is exactly one definition of each, rather than
// two copies free to drift.
package fakewire

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// JSONL turn types the broker's classifier keys on.
const (
	AssistantTurnType = "assistant"
	ResultTurnType    = "result"
)

// RateLimitLine is the shape claude emits when Anthropic returns HTTP 429 — the
// authoritative backpressure signal the AIMD controller (🎯T2.2) keys on, and
// the exact line broker.ClassifyResultLine must recognise.
const RateLimitLine = `{"type":"result","subtype":"error","is_error":true,` +
	`"error":{"type":"rate_limit_error","status":429,"message":"rate limited"}}`

// UsageBlock is one canned token-usage record, the unit 🎯T2.4's cost
// differential consumes.
type UsageBlock struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

// Readiness selects which terminal frame the fake writes to stderr, standing in
// for what the real claude TUI renders in its tmux pane.
type Readiness string

// Readiness frames.
const (
	// ReadyNone writes no frame at all.
	ReadyNone Readiness = ""
	// ReadyPromptBox writes the live idle input box — the frame
	// tmuxagent.MatchReady accepts.
	ReadyPromptBox Readiness = "prompt_box"
	// ReadySplash writes the startup splash: the same box drawn around the
	// ghost placeholder, before input is wired up. MatchReady must reject it
	// (🎯T284), so a policy loop that fires on this frame is racing the TUI.
	ReadySplash Readiness = "splash"
)

// Scenario describes one fake claude run.
type Scenario struct {
	Ready       Readiness    `json:"ready"`
	ReadyMarker string       `json:"ready_marker"`
	Usage       []UsageBlock `json:"usage"`
	Lines       []string     `json:"lines"`
	CostUSD     float64      `json:"cost_usd"`
	RateLimited bool         `json:"rate_limited"`
	ExitCode    int          `json:"exit_code"`
}

// Prompt-box glyphs, spelled once. The rule is U+2500 BOX DRAWINGS LIGHT
// HORIZONTAL and the prompt is U+276F HEAVY RIGHT-POINTING ANGLE QUOTATION MARK
// ORNAMENT, padded with the U+00A0 NBSP claude renders — the exact characters
// tmuxagent's readiness regexes match on.
const (
	boxRule     = "──────────────────────────────────────────"
	promptGlyph = "❯"
	// nbsp is spelled as an escape because it is invisible and load-bearing:
	// tmuxagent's splash detector anchors on the NBSP claude pads the prompt
	// glyph with, and a plain space here would silently break the match.
	nbsp = "\u00A0"
	// statusLine is one of the trailing lines the real TUI prints under the
	// box; the readiness pattern tolerates a few, so the fake includes one to
	// keep the frame honest rather than minimal.
	statusLine = "  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	// splashHint is claude's ghost placeholder, drawn inside the box before
	// input is wired up.
	splashHint = `Try "fix lint errors"`
)

// PromptBoxFrame is a captured-pane frame showing the live idle input box.
func PromptBoxFrame() []byte {
	return []byte(strings.Join([]string{
		"● Ready.",
		boxRule,
		promptGlyph + nbsp,
		boxRule,
		statusLine,
		"",
	}, "\n"))
}

// SplashFrame is a captured-pane frame showing the same box around the startup
// ghost placeholder — drawn, but not yet accepting input.
func SplashFrame() []byte {
	return []byte(strings.Join([]string{
		"✻ Welcome to Claude Code",
		boxRule,
		promptGlyph + nbsp + splashHint,
		boxRule,
		statusLine,
		"",
	}, "\n"))
}

// Render writes one scenario's output and returns the process exit code. It is
// the fake's entire behaviour, kept pure over its writers so it can be
// exercised in-process as well as through the compiled binary.
func Render(s Scenario, stdout, stderr io.Writer) int {
	// The readiness frame goes to stderr so it never pollutes the JSONL
	// stream, mirroring the real split between the TUI pane and --output-format
	// stream-json.
	switch {
	case s.Ready == ReadyPromptBox:
		stderr.Write(PromptBoxFrame())
	case s.Ready == ReadySplash:
		stderr.Write(SplashFrame())
	case s.ReadyMarker != "":
		fmt.Fprintln(stderr, s.ReadyMarker)
	}

	if s.RateLimited {
		fmt.Fprintln(stdout, RateLimitLine)
		if s.ExitCode == 0 {
			return 1
		}
		return s.ExitCode
	}

	for _, u := range s.Usage {
		turn := map[string]any{
			"type":    AssistantTurnType,
			"message": map[string]any{"usage": u},
		}
		b, err := json.Marshal(turn)
		if err != nil {
			fmt.Fprintln(stderr, "fake-claude:", err)
			return 2
		}
		fmt.Fprintln(stdout, string(b))
	}

	for _, line := range s.Lines {
		fmt.Fprintln(stdout, line)
	}

	// Close the run with a success result whenever the scenario described a
	// billable turn, so a cost oracle sees the same shape a real run produces.
	if len(s.Usage) > 0 || s.CostUSD != 0 {
		b, err := json.Marshal(map[string]any{
			"type":     ResultTurnType,
			"subtype":  "success",
			"is_error": false,
			"cost_usd": s.CostUSD,
		})
		if err != nil {
			fmt.Fprintln(stderr, "fake-claude:", err)
			return 2
		}
		fmt.Fprintln(stdout, string(b))
	}

	return s.ExitCode
}
