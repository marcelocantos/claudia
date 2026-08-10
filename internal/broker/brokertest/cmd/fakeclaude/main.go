// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command fakeclaude is a behavioural stand-in for the `claude` binary used by
// the broker's oracle harness. It is never shipped; brokertest.Build compiles
// it into a temp dir named "claude" so code under test can exec it with no API
// credit and no real binary. Its behaviour is driven entirely by a scenario
// file referenced via the FAKE_CLAUDE_SCENARIO environment variable, and is
// rendered by fakewire — the same package the helper and the broker's
// classifier agree with on wire shapes.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marcelocantos/claudia/internal/broker/brokertest/fakewire"
)

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
	var s fakewire.Scenario
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintln(os.Stderr, "fake-claude:", err)
		return 2
	}
	return fakewire.Render(s, os.Stdout, os.Stderr)
}
