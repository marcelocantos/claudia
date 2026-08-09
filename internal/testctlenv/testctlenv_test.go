// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package testctlenv

import (
	"slices"
	"strings"
	"testing"
)

// TestRegistryMembershipPinned pins the registry in both directions.
//
// Under-broad is the leak this package exists to stop. Over-broad is
// the subtler failure: widening the strip to a CLAUDIA_* wildcard
// would silently swallow the variables that tell an agent how to reach
// its provider, and the agent would fail for reasons no test explains.
// Both halves must be asserted, so neither drift direction is free.
func TestRegistryMembershipPinned(t *testing.T) {
	mustStrip := []string{
		"CLAUDIA_CRASH_TEST_HELPER",
		"CLAUDIA_CRASH_TEST_WORKDIR",
		"CLAUDIA_POOL_CRASH_HELPER",
		"CLAUDIA_POOL_CRASH_WORKDIR",
		"CLAUDIA_LIVE",
		"CLAUDIA_BEDROCK_LIVE",
		"CLAUDIA_CODEX_LIVE",
		"CLAUDIA_GROK_LIVE",
	}
	// Configuration an agent legitimately needs. Stripping any of
	// these breaks the agent instead of protecting it.
	mustKeep := []string{
		"CLAUDIA_TMUX_SOCKET",
		"CLAUDIA_BEDROCK_REGION",
		"CLAUDIA_BEDROCK_MODEL_ID",
		"CLAUDIA_CLAUDE_OAUTH_TOKEN",
		"CLAUDIA_CODEX_ACCESS_TOKEN",
		"CLAUDIA_CODEX_ACCOUNT_ID",
		"CLAUDIA_CODEX_AUTH_PATH",
		"CLAUDIA_GROK_CONNECT",
	}

	all := All()
	for _, name := range mustStrip {
		if !slices.Contains(all, name) {
			t.Errorf("%s is a test-control variable but All() omits it — it would leak into agents", name)
		}
	}
	for _, name := range mustKeep {
		if slices.Contains(all, name) {
			t.Errorf("%s is agent configuration, not test control — stripping it breaks the agent", name)
		}
	}
	if len(all) != len(mustStrip) {
		t.Errorf("All() has %d entries, pinned list has %d: %v", len(all), len(mustStrip), all)
	}

	// Helpers are the tier the process-start oracle hard-fails on, so
	// membership there is load-bearing beyond All().
	for _, name := range Helpers() {
		if !strings.HasSuffix(name, "_HELPER") && !strings.HasSuffix(name, "_WORKDIR") {
			t.Errorf("Helpers() contains %s, which is not a parent/child handshake variable", name)
		}
	}
}

func TestStripRemovesOnlyTestControl(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"CLAUDIA_CRASH_TEST_HELPER=1",
		"CLAUDIA_TMUX_SOCKET=/tmp/x.sock",
		"CLAUDIA_LIVE=1",
		"HOME=/home/u",
		"CLAUDIA_CRASH_TEST_WORKDIR=/tmp/stale",
		// A variable that merely contains a registry name as a prefix
		// must survive: matching is on the whole name, not a prefix.
		"CLAUDIA_LIVE_EXTRA=keep",
	}
	got := Strip(env)
	want := []string{
		"PATH=/usr/bin",
		"CLAUDIA_TMUX_SOCKET=/tmp/x.sock",
		"HOME=/home/u",
		"CLAUDIA_LIVE_EXTRA=keep",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Strip:\n got %v\nwant %v", got, want)
	}
}

func TestLeakedHelpersReportsPresentEvenWhenEmpty(t *testing.T) {
	// An exported-but-empty variable is still a leak: tmux's `-e VAR=`
	// sets an empty value rather than unsetting, so treating empty as
	// absent would call a half-closed leak clean.
	set := map[string]string{"CLAUDIA_POOL_CRASH_HELPER": ""}
	lookup := func(name string) (string, bool) {
		v, ok := set[name]
		return v, ok
	}
	got := LeakedHelpers(lookup)
	if !slices.Equal(got, []string{"CLAUDIA_POOL_CRASH_HELPER"}) {
		t.Errorf("LeakedHelpers = %v, want [CLAUDIA_POOL_CRASH_HELPER]", got)
	}
	if leaked := LeakedHelpers(func(string) (string, bool) { return "", false }); leaked != nil {
		t.Errorf("LeakedHelpers on clean env = %v, want nil", leaked)
	}
}
