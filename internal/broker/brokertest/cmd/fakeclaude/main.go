// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command fakeclaude is a behavioural stand-in for the `claude` binary used by
// the broker's oracle harness. It is never shipped; brokertest.Build compiles
// it into a temp dir named "claude" so code under test can exec it with no API
// credit and no real binary. Its behaviour is driven entirely by a scenario
// file referenced via the FAKE_CLAUDE_SCENARIO environment variable.
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// scenario mirrors brokertest.Scenario. Kept as a local copy so the fake binary
// has no dependency on the test-only helper package.
type scenario struct {
	ReadyMarker string   `json:"ready_marker"`
	Lines       []string `json:"lines"`
	RateLimited bool     `json:"rate_limited"`
	ExitCode    int      `json:"exit_code"`
}

// rateLimitLine is the shape claude emits when Anthropic returns HTTP 429 — the
// authoritative backpressure signal the AIMD controller (T2.2) keys on.
const rateLimitLine = `{"type":"result","subtype":"error","is_error":true,` +
	`"error":{"type":"rate_limit_error","status":429,"message":"rate limited"}}`

func main() {
	os.Exit(run())
}

func run() int {
	path := os.Getenv("FAKE_CLAUDE_SCENARIO")
	if path == "" {
		fmt.Fprintln(os.Stderr, "fake-claude: FAKE_CLAUDE_SCENARIO not set")
		return 2
	}
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-claude:", err)
		return 2
	}
	var s scenario
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintln(os.Stderr, "fake-claude:", err)
		return 2
	}

	// The readiness marker stands in for the prompt-box pattern the real TUI
	// prints; it goes to stderr so it never pollutes the JSONL stream.
	if s.ReadyMarker != "" {
		fmt.Fprintln(os.Stderr, s.ReadyMarker)
	}

	if s.RateLimited {
		fmt.Fprintln(os.Stdout, rateLimitLine)
		if s.ExitCode == 0 {
			return 1
		}
		return s.ExitCode
	}

	for _, line := range s.Lines {
		fmt.Fprintln(os.Stdout, line)
	}
	return s.ExitCode
}
