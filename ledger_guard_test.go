// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsBullseyeYAML(t *testing.T) {
	if !IsBullseyeYAML("/Users/x/work/jevons/bullseye.yaml") {
		t.Fatal("abs path")
	}
	if !IsBullseyeYAML("bullseye.yaml") {
		t.Fatal("base")
	}
	if IsBullseyeYAML("agents-guide.md") || IsBullseyeYAML("") {
		t.Fatal("false positive")
	}
}

func TestSelectPermissionRejectsCursorStrReplaceOfLedger(t *testing.T) {
	params := json.RawMessage(`{
		"sessionId": "sid",
		"toolCall": {
			"toolCallId": "tc-1",
			"title": "StrReplace",
			"kind": "edit",
			"locations": [{"path": "/Users/marcelo/work/github.com/marcelocantos/jevons/bullseye.yaml"}],
			"rawInput": {"path": "/Users/marcelo/work/github.com/marcelocantos/jevons/bullseye.yaml"}
		},
		"options": [
			{"optionId": "allow-once"},
			{"optionId": "allow-always"},
			{"optionId": "reject-once"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "reject-once" {
		t.Fatalf("got %q, want reject-once (🎯T546)", got)
	}
	reply := permissionSelectedReply(params)
	outcome, _ := reply["outcome"].(map[string]any)
	if outcome["optionId"] != "reject-once" {
		t.Fatalf("reply optionId = %v, want reject-once", outcome["optionId"])
	}
	meta, _ := reply["_meta"].(map[string]any)
	reason, _ := meta["reason"].(string)
	if !strings.Contains(reason, "jevons_target_file") || !strings.Contains(reason, "T546") {
		t.Fatalf("permission refuse must name the bullseye tool: %q", reason)
	}
}

func TestSelectPermissionAllowsCursorEditOfOtherFile(t *testing.T) {
	params := json.RawMessage(`{
		"toolCall": {
			"title": "StrReplace",
			"kind": "edit",
			"locations": [{"path": "/tmp/foo.go"}]
		},
		"options": [
			{"optionId": "allow-once"},
			{"optionId": "allow-always"},
			{"optionId": "reject-once"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "allow-always" {
		t.Fatalf("got %q, want allow-always", got)
	}
}

func TestSelectPermissionAllowsReadOfLedger(t *testing.T) {
	params := json.RawMessage(`{
		"toolCall": {
			"title": "Read",
			"kind": "read",
			"locations": [{"path": "/tmp/bullseye.yaml"}]
		},
		"options": [
			{"optionId": "allow-once"},
			{"optionId": "allow-always"},
			{"optionId": "reject-once"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "allow-always" {
		t.Fatalf("Read of ledger must not hit the mutate refuse: got %q", got)
	}
}
