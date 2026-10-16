// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"
)

func TestParseCursorTaskLineResultUsageCamelCase(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":false,"duration_ms":50,"result":"PING","usage":{"inputTokens":11,"outputTokens":3,"cacheReadTokens":2,"cacheWriteTokens":1}}`)
	evs := ParseCursorTaskLine(line)
	if len(evs) != 1 || evs[0].Type != TaskEventResult {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Content != "PING" {
		t.Fatalf("content = %q", evs[0].Content)
	}
	if evs[0].Usage.InputTokens != 11 || evs[0].Usage.OutputTokens != 3 {
		t.Fatalf("usage = %+v", evs[0].Usage)
	}
	if evs[0].Usage.CacheReadInputTokens != 2 || evs[0].Usage.CacheCreationInputTokens != 1 {
		t.Fatalf("cache usage = %+v", evs[0].Usage)
	}
}

func TestParseCursorTaskLineError(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","is_error":true,"result":"bad model"}`)
	evs := ParseCursorTaskLine(line)
	if len(evs) != 1 || evs[0].Type != TaskEventError || evs[0].ErrorMsg != "bad model" {
		t.Fatalf("events = %+v", evs)
	}
}
