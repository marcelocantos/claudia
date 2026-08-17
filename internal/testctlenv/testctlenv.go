// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package testctlenv is the canonical registry of claudia's
// test-control environment variables: the variables whose mere
// presence changes which tests run, or what they do, rather than
// configuring the product.
//
// It exists because claudia spawns agents inside a long-lived tmux
// server, and a tmux server freezes the environment of whichever
// process started it as its global environment. Crash-survival tests
// deliberately outlive their own process, so a test-control variable
// set for a helper subprocess can end up baked into the claudia tmux
// server and inherited by every agent window created afterwards —
// possibly for days. A fleet agent running `go test ./...` in such a
// window then runs a different suite from the one the owner runs, and
// reports green for it. See target T20.
//
// The registry is split into two tiers because they warrant different
// enforcement:
//
//   - Helper variables are never legitimately set by a human. They are
//     private handshakes between a parent test and the child process it
//     spawns, and a helper test un-skipped outside that handshake blocks
//     forever. Their presence at process start is always a leak, so the
//     process-start oracle hard-fails on them.
//
//   - Live gates are legitimately set by a human who wants the live
//     tests to run (`CLAUDIA_LIVE=1 go test ./...`). Their presence at
//     process start cannot be distinguished from a deliberate choice,
//     so the oracle does not fail on them — but they are still stripped
//     from spawned agent environments, so that a fleet agent never
//     inherits them ambiently from whoever launched the harness.
//
// Both tiers are stripped at agent spawn. Only the helper tier is
// asserted-unset at process start.
package testctlenv

import "strings"

// Helpers are the parent-to-child handshake variables. A test guarded
// by one of these blocks forever when it is un-skipped outside the
// handshake, so their presence in an ambient environment is always a
// bug. Keep in sync with the guards in agent_crash_test.go and
// pool_test.go.
func Helpers() []string {
	return []string{
		"CLAUDIA_CRASH_TEST_HELPER",
		"CLAUDIA_CRASH_TEST_WORKDIR",
		"CLAUDIA_POOL_CRASH_HELPER",
		"CLAUDIA_POOL_CRASH_WORKDIR",
	}
}

// LiveGates un-skip tests that spend API credit or need a real
// provider binary. A human may set these deliberately; an agent must
// never inherit them by accident.
func LiveGates() []string {
	return []string{
		"CLAUDIA_LIVE",
		"CLAUDIA_BEDROCK_LIVE",
		"CLAUDIA_CODEX_LIVE",
		"CLAUDIA_GROK_LIVE",
		"CLAUDIA_OLLAMA_LIVE",
	}
}

// All is every test-control variable: the set stripped from any
// environment claudia hands to a spawned agent.
//
// Deliberately NOT included are claudia's configuration variables —
// CLAUDIA_TMUX_SOCKET, CLAUDIA_BEDROCK_REGION, CLAUDIA_BEDROCK_MODEL_ID,
// CLAUDIA_CLAUDE_OAUTH_TOKEN, CLAUDIA_CODEX_ACCESS_TOKEN,
// CLAUDIA_CODEX_ACCOUNT_ID, CLAUDIA_CODEX_AUTH_PATH,
// CLAUDIA_GROK_CONNECT. Those tell an agent how to reach its provider;
// stripping them would break the agent rather than protect it. The
// registry is an explicit list, not a CLAUDIA_* wildcard, for exactly
// this reason.
func All() []string {
	return append(Helpers(), LiveGates()...)
}

// Strip returns env (in os.Environ() "K=V" form) with every
// test-control variable removed.
func Strip(env []string) []string {
	drop := make(map[string]bool, len(All()))
	for _, name := range All() {
		drop[name] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok && drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// LeakedHelpers returns the helper variables that lookup reports as
// present. lookup has os.LookupEnv's signature. A non-empty result
// means a parent-to-child handshake variable escaped its subprocess.
func LeakedHelpers(lookup func(string) (string, bool)) []string {
	var leaked []string
	for _, name := range Helpers() {
		if _, ok := lookup(name); ok {
			leaked = append(leaked, name)
		}
	}
	return leaked
}
