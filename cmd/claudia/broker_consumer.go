// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/marcelocantos/claudia/daemon"
)

// These are broker-process settings: setting them in a client cannot
// reconfigure an already running shared broker.
func bindMCPConsumerFlags(fs *flag.FlagSet, opts *daemon.Options) {
	fs.Func("mcp-seed-dir", "prior MCP owner's state directory (repeatable; default none)", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("MCP seed directory must not be empty")
		}
		opts.MCPSeedStateDirs = append(opts.MCPSeedStateDirs, value)
		return nil
	})
	fs.Func("mcp-consumer-prefix", "MCP server name prefix left on the consumer's URL (repeatable; default none)", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("MCP consumer prefix must not be empty")
		}
		opts.MCPConsumerOwnedPrefixes = append(opts.MCPConsumerOwnedPrefixes, value)
		return nil
	})
	fs.StringVar(&opts.ConsumerStateDir, "consumer-state-dir", "", "Jevons state directory; fleet/intent.json is merged into claims at boot so parked/reaped seats are not resumed (🎯T207); default CLAUDIA_CONSUMER_STATE_DIR")
}
