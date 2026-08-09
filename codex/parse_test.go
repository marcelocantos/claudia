// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func readFixtureLines(t *testing.T, rel string) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", rel))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines [][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestParseSuccessFixture(t *testing.T) {
	events := ParseLines(readFixtureLines(t, "exec/success.jsonl"))
	if len(events) != 5 {
		t.Fatalf("events = %d, want 5: %#v", len(events), events)
	}
	if events[0].Type != EventInit || events[0].SessionID == "" {
		t.Errorf("init = %#v", events[0])
	}
	if events[1].Type != EventToolUse || events[1].ToolName != "command_execution" {
		t.Errorf("tool = %#v", events[1])
	}
	if events[2].Type != EventText || events[2].Content == "" {
		t.Errorf("text = %#v", events[2])
	}
	if events[3].Type != EventText || events[3].Content != "Final answer." {
		t.Errorf("final text = %#v", events[3])
	}
	res := events[4]
	if res.Type != EventResult || res.Content != "Final answer." {
		t.Fatalf("result = %#v", res)
	}
	if res.Usage.InputTokens != 24763 || res.Usage.CacheReadInputTokens != 24448 || res.Usage.OutputTokens != 122 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestParseFailureAndErrorFixtures(t *testing.T) {
	for _, fixture := range []string{"exec/failure.jsonl", "exec/error.jsonl"} {
		events := ParseLines(readFixtureLines(t, fixture))
		if len(events) == 0 {
			t.Fatalf("%s: no events", fixture)
		}
		ev := events[len(events)-1]
		if ev.Type != EventError || ev.Error == nil {
			t.Errorf("%s: last event = %#v", fixture, ev)
		}
	}
}

func TestParseRateLimitFixture(t *testing.T) {
	events := ParseLines(readFixtureLines(t, "exec/rate_limit.jsonl"))
	var saw *RateLimitError
	for _, ev := range events {
		if ev.Type != EventError {
			continue
		}
		if errors.As(ev.Error, &saw) {
			break
		}
	}
	if saw == nil {
		t.Fatalf("events = %#v, want *RateLimitError", events)
	}
}

func TestParseAuthFailFixture(t *testing.T) {
	events := ParseLines(readFixtureLines(t, "exec/auth_fail.jsonl"))
	if len(events) != 1 || events[0].Type != EventError {
		t.Fatalf("events = %#v", events)
	}
	var ae *AuthError
	if !errors.As(events[0].Error, &ae) {
		t.Fatalf("error = %T %v, want *AuthError", events[0].Error, events[0].Error)
	}
}

func TestParseMalformedFixture(t *testing.T) {
	for _, line := range readFixtureLines(t, "exec/malformed.jsonl") {
		var p parser
		if got := p.ParseLine(line); len(got) != 0 {
			t.Errorf("ParseLine(%q) = %#v, want none", line, got)
		}
	}
}

func TestSuccessOracleRejectsFaults(t *testing.T) {
	lines := readFixtureLines(t, "exec/success.jsonl")
	if err := successOracle(lines); err != nil {
		t.Fatalf("success oracle: %v", err)
	}
	// Drop thread.started
	if err := successOracle(lines[1:]); err == nil {
		t.Fatal("expected session failure after dropping thread.started")
	}
	// Wrong final message
	mut := append([][]byte(nil), lines...)
	mut[4] = []byte(`{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Stale answer."}}`)
	if err := successOracle(mut); err == nil {
		t.Fatal("expected final-message failure")
	}
}

func successOracle(lines [][]byte) error {
	events := ParseLines(lines)
	var sawSession, sawResult bool
	var result Event
	for _, ev := range events {
		if ev.Type == EventInit && ev.SessionID != "" {
			sawSession = true
		}
		if ev.Type == EventResult {
			sawResult = true
			result = ev
		}
	}
	if !sawSession {
		return errors.New("missing session id")
	}
	if !sawResult {
		return errors.New("missing final result")
	}
	if result.Content != "Final answer." {
		return errors.New("wrong final message")
	}
	if result.Usage.InputTokens != 24763 ||
		result.Usage.CacheReadInputTokens != 24448 ||
		result.Usage.OutputTokens != 122 {
		return errors.New("malformed usage accounting")
	}
	return nil
}
