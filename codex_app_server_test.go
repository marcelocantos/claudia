// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "testing"

func TestParseCodexAppServerSuccessFixture(t *testing.T) {
	var threadID, turnID, command, finalText string
	var usage Usage
	var completed bool
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/success.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexAppServerLine(%q): %v", line, err)
		}
		if !ok {
			continue
		}
		if ev.ThreadID != "" {
			threadID = ev.ThreadID
		}
		if ev.TurnID != "" {
			turnID = ev.TurnID
		}
		if ev.ItemType == "command_execution" {
			command = ev.Command
		}
		if ev.ItemType == "agent_message" {
			finalText = ev.Text
		}
		if ev.Method == "turn/completed" {
			completed = true
			usage = ev.Usage
		}
	}
	if threadID != "thr_success" {
		t.Fatalf("threadID = %q, want thr_success", threadID)
	}
	if turnID != "turn_success" {
		t.Fatalf("turnID = %q, want turn_success", turnID)
	}
	if command != "bash -lc ls" {
		t.Fatalf("command = %q, want bash -lc ls", command)
	}
	if finalText != "Final answer." {
		t.Fatalf("finalText = %q, want Final answer.", finalText)
	}
	if !completed {
		t.Fatal("success fixture did not produce turn/completed")
	}
	if usage.InputTokens != 10 || usage.CacheReadInputTokens != 4 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestParseCodexAppServerFailureFixture(t *testing.T) {
	var failed bool
	var errorMsg string
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/failure.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexAppServerLine(%q): %v", line, err)
		}
		if ok && ev.Method == "turn/completed" {
			failed = ev.Status == "failed"
			errorMsg = ev.ErrorMsg
		}
	}
	if !failed {
		t.Fatal("failure fixture did not produce failed turn/completed")
	}
	if errorMsg != "model failed" {
		t.Fatalf("errorMsg = %q, want model failed", errorMsg)
	}
}

func TestParseCodexAppServerInterruptedFixture(t *testing.T) {
	var interrupted bool
	for _, line := range readFixtureLines(t, "testdata/codex/app-server/interrupted.jsonl") {
		ev, ok, err := parseCodexAppServerLine([]byte(line))
		if err != nil {
			t.Fatalf("parseCodexAppServerLine(%q): %v", line, err)
		}
		if ok && ev.Method == "turn/completed" {
			interrupted = ev.Status == "interrupted"
		}
	}
	if !interrupted {
		t.Fatal("interrupted fixture did not produce interrupted turn/completed")
	}
}

func TestParseCodexAppServerUnsupportedCapabilityFixture(t *testing.T) {
	line := readFixtureLines(t, "testdata/codex/app-server/unsupported-capability.jsonl")[0]
	ev, ok, err := parseCodexAppServerLine([]byte(line))
	if err != nil {
		t.Fatalf("parseCodexAppServerLine: %v", err)
	}
	if !ok {
		t.Fatal("unsupported-capability fixture was ignored")
	}
	if !ev.IsError || !ev.IsResponse {
		t.Fatalf("event = %+v, want response error", ev)
	}
	if ev.ErrorMsg != "requires experimentalApi capability" {
		t.Fatalf("ErrorMsg = %q, want requires experimentalApi capability", ev.ErrorMsg)
	}
}

func TestParseCodexAppServerMalformedLine(t *testing.T) {
	if _, _, err := parseCodexAppServerLine([]byte(`{"method":`)); err == nil {
		t.Fatal("malformed line returned nil error")
	}
}
