// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"encoding/json"
	"errors"
	"testing"
)

type stubCause struct {
	cause string
	text  string
}

func (e stubCause) Error() string         { return e.text }
func (e stubCause) ProviderCause() string { return e.cause }

func TestAgentFailedCauseSurvivesTheWire(t *testing.T) {
	pe := agentFailed(stubCause{cause: "turn_in_flight", text: "grok acp: prompt already in flight"})
	if pe.Field != "" {
		t.Fatalf("field = %q", pe.Field)
	}
	if pe.Error() != "broker protocol: agent_failed: grok acp: prompt already in flight" {
		t.Fatalf("rendered = %q", pe.Error())
	}
	raw, err := json.Marshal(pe.Wire())
	if err != nil {
		t.Fatal(err)
	}
	var msg ErrorMessage
	if err := decodeStrict(raw, &msg, "error"); err != nil {
		t.Fatal(err)
	}
	got := msg.Err()
	if got.ProviderCause() != "turn_in_flight" {
		t.Fatalf("cause = %q", got.ProviderCause())
	}
	wrapped := agentFailed(errors.Join(errors.New("send"), stubCause{cause: "process_not_running", text: "claude process not running"}))
	// errors.Join unwraps. The cause is the typed one, not the joined sentence.
	if wrapped.ProviderCause() != "process_not_running" {
		t.Fatalf("wrapped cause = %q", wrapped.ProviderCause())
	}

	plain := agentFailed(errors.New("401 unauthorized"))
	if plain.ProviderCause() != "" || plain.Value != "" {
		t.Fatalf("prose invented a cause: %+v", plain)
	}
	if plain.Error() != "broker protocol: agent_failed: 401 unauthorized" {
		t.Fatalf("rendered = %q", plain.Error())
	}

	other := &ProtocolError{Code: CodeUnsupportedValue, Field: "mode", Value: "nudge", Msg: "no"}
	if other.ProviderCause() != "" {
		t.Fatalf("offending value reported as a cause: %q", other.ProviderCause())
	}
}
