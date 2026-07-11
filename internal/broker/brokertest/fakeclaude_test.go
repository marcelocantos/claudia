// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package brokertest

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// These tests are the fake's own oracle: they prove the double actually emits
// the three signals the broker's policy loops consume — canned JSONL turns
// (cost tracking, T2.4), an injectable 429 (AIMD, T2.2), and a readiness marker
// (spawn/pool, T2.1/T2.3).

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
