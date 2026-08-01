// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"testing"
)

func TestSelectPermissionOptionIDBashSpecific(t *testing.T) {
	// Live Grok shape for run_terminal_command: no generic allow_always.
	params := json.RawMessage(`{
		"options": [
			{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"},
			{"optionId": "allow_always_bash", "name": "Always allow bash", "kind": "allow_always"},
			{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
			{"optionId": "reject_always_bash", "name": "Never bash", "kind": "reject_always"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "allow_always_bash" {
		t.Fatalf("got %q, want allow_always_bash (must be offered for shell)", got)
	}
}

func TestSelectPermissionOptionIDPrefersGenericAlways(t *testing.T) {
	params := json.RawMessage(`{
		"options": [
			{"optionId": "allow_once"},
			{"optionId": "allow_always"},
			{"optionId": "allow_always_bash"}
		]
	}`)
	got := selectPermissionOptionID(params)
	if got != "allow_always" {
		t.Fatalf("got %q, want allow_always", got)
	}
}

func TestSelectPermissionOptionIDAllowOnceOnly(t *testing.T) {
	params := json.RawMessage(`{"options":[{"optionId":"allow_once"},{"optionId":"reject_once"}]}`)
	got := selectPermissionOptionID(params)
	if got != "allow_once" {
		t.Fatalf("got %q, want allow_once", got)
	}
}

func TestSelectPermissionOptionIDEmptyFallback(t *testing.T) {
	if got := selectPermissionOptionID(nil); got != "allow_always" {
		t.Fatalf("nil: got %q", got)
	}
	if got := selectPermissionOptionID(json.RawMessage(`{}`)); got != "allow_always" {
		t.Fatalf("empty object: got %q", got)
	}
	if got := selectPermissionOptionID(json.RawMessage(`{"options":[]}`)); got != "allow_always" {
		t.Fatalf("empty options: got %q", got)
	}
}

func TestSelectPermissionOptionIDRejectsOnlyPicksOffered(t *testing.T) {
	// Degenerate: only rejects offered — still must not invent allow_always.
	params := json.RawMessage(`{"options":[{"optionId":"reject_once"},{"optionId":"reject_always_bash"}]}`)
	got := selectPermissionOptionID(params)
	if got != "reject_once" && got != "reject_always_bash" {
		t.Fatalf("got %q, want one of the offered reject ids", got)
	}
}

func TestPermissionOptionIDsParse(t *testing.T) {
	params := json.RawMessage(`{"sessionId":"s","options":[{"optionId":"a"},{"optionId":""},{"optionId":"b"}]}`)
	ids := permissionOptionIDs(params)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids = %#v", ids)
	}
}
