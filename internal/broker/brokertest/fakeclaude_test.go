// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package brokertest

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// These tests are the fake's own oracle: they prove the double actually emits
// the signals the broker's policy loops consume — canned JSONL turns and usage
// blocks (cost tracking, 🎯T2.4), an injectable 429 (AIMD, 🎯T2.2), and the real
// readiness prompt-box pattern (spawn/pool, 🎯T2.1/🎯T2.3). A double that emits
// something the product would never emit is not a referent, so each assertion
// below is against the shape the product side actually parses.

func TestFakeClaudeEmitsCannedJSONL(t *testing.T) {
	f := Build(t)
	out, err := f.Command(t, Scenario{
		ReadyMarker: "● ready",
		Lines: []string{
			`{"type":"assistant","usage":{"input_tokens":10,"output_tokens":5}}`,
			`{"type":"result","subtype":"success","cost_usd":0.0123}`,
		},
	}).Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"output_tokens":5`) || !strings.Contains(s, `"cost_usd":0.0123`) {
		t.Fatalf("stdout missing canned JSONL:\n%s", s)
	}
}

func TestFakeClaudeInjects429(t *testing.T) {
	f := Build(t)
	out, err := f.Command(t, Scenario{RateLimited: true}).CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("rate-limited run should exit non-zero, got err=%v", err)
	}
	if !strings.Contains(string(out), "rate_limit_error") || !strings.Contains(string(out), "429") {
		t.Fatalf("output missing 429 markers:\n%s", out)
	}
}

func TestFakeClaudeReadyMarkerOnStderr(t *testing.T) {
	f := Build(t)
	cmd := f.Command(t, Scenario{ReadyMarker: "PROMPT-READY"})
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stderr.String(), "PROMPT-READY") {
		t.Fatalf("readiness marker not on stderr:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "PROMPT-READY") {
		t.Fatal("readiness marker leaked into the JSONL stdout stream")
	}
}

// TestFakeClaudeUsageBlocks proves the canned usage turns round-trip through
// the same parse the cost estimator will use, and that the run closes with a
// success result carrying the scenario's cost.
func TestFakeClaudeUsageBlocks(t *testing.T) {
	f := Build(t)
	out, err := f.Command(t, Scenario{
		Usage: []UsageBlock{
			{InputTokens: 1200, OutputTokens: 340, CacheReadTokens: 900},
			{InputTokens: 80, OutputTokens: 12, CacheCreationTokens: 64},
		},
		CostUSD: 0.0451,
	}).Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := ParseUsage(t, out)
	if len(got) != 2 {
		t.Fatalf("parsed %d usage blocks, want 2; raw output:\n%s", len(got), out)
	}
	if got[0].InputTokens != 1200 || got[0].CacheReadTokens != 900 {
		t.Errorf("block 0 = %+v, want input 1200 / cache-read 900", got[0])
	}
	if got[1].OutputTokens != 12 || got[1].CacheCreationTokens != 64 {
		t.Errorf("block 1 = %+v, want output 12 / cache-creation 64", got[1])
	}
	if !strings.Contains(string(out), `"cost_usd":0.0451`) {
		t.Errorf("closing result line missing the scenario cost:\n%s", out)
	}
}

// TestFakeClaudeReadinessFramesMatchTheRealMatcher is the load-bearing one: the
// frames the fake writes are checked against tmuxagent's real readiness
// matcher, the same code the pool's warm-acquire path calls. Both directions
// matter — the prompt box must read ready, and the startup splash must NOT
// (🎯T284: the splash draws an identical box while input is still dead, and a
// policy loop that fires on it races the TUI).
func TestFakeClaudeReadinessFramesMatchTheRealMatcher(t *testing.T) {
	if !tmuxagent.MatchReady(PromptBoxFrame()) {
		t.Errorf("prompt-box frame is not accepted by tmuxagent.MatchReady:\n%s", PromptBoxFrame())
	}
	if tmuxagent.MatchReady(SplashFrame()) {
		t.Errorf("startup splash frame was accepted as ready:\n%s", SplashFrame())
	}
	if !tmuxagent.MatchStartupSplash(SplashFrame()) {
		t.Errorf("startup splash frame is not recognised as a splash:\n%s", SplashFrame())
	}
}

// TestFakeClaudeWritesReadinessFrameToStderr proves the selected frame reaches
// stderr verbatim, so a harness capturing the fake's pane sees exactly what the
// matcher was tested against.
func TestFakeClaudeWritesReadinessFrameToStderr(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready Readiness
		frame []byte
	}{
		{"prompt box", ReadyPromptBox, PromptBoxFrame()},
		{"splash", ReadySplash, SplashFrame()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := Build(t)
			cmd := f.Command(t, Scenario{Ready: tc.ready})
			var stdout, stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("run: %v", err)
			}
			if stderr.String() != string(tc.frame) {
				t.Fatalf("stderr frame mismatch:\ngot:\n%s\nwant:\n%s", stderr.String(), tc.frame)
			}
			if stdout.String() != "" {
				t.Fatalf("readiness frame leaked into stdout:\n%s", stdout.String())
			}
		})
	}
}
