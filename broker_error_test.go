// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"fmt"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

func TestAgentFailedTypedProtocolError(t *testing.T) {
	t.Parallel()
	pe := &broker.ProtocolError{Code: broker.CodeAgentFailed, Msg: "cursor acp: prompt already in flight"}
	msg, ok := AgentFailed(pe)
	if !ok || msg != pe.Msg {
		t.Fatalf("AgentFailed(%v) = %q, %v; want %q, true", pe, msg, ok, pe.Msg)
	}
	wrapped := fmt.Errorf("send: %w", pe)
	msg, ok = AgentFailed(wrapped)
	if !ok || msg != pe.Msg {
		t.Fatalf("AgentFailed(wrapped) = %q, %v; want %q, true", msg, ok, pe.Msg)
	}
}

func TestAgentFailedRejectsOtherCodesAndProse(t *testing.T) {
	t.Parallel()
	for i, err := range []error{
		nil,
		errors.New("broker protocol: agent_failed: cursor acp: prompt already in flight"),
		errors.New("provider response quoted: grok acp: prompt already in flight"),
		&broker.ProtocolError{Code: broker.CodeMalformed, Msg: "cursor acp: prompt already in flight"},
		&broker.ProtocolError{Code: broker.CodeNotAvailable, Msg: "no daemon"},
	} {
		if msg, ok := AgentFailed(err); ok {
			t.Fatalf("case %d AgentFailed(%v) = %q, true; want ok=false", i, err, msg)
		}
	}
}

func TestAgentFailedWireRoundTrip(t *testing.T) {
	t.Parallel()
	wire := (&broker.ProtocolError{Code: broker.CodeAgentFailed, Msg: "claude process not running"}).Wire()
	pe := wire.Err()
	msg, ok := AgentFailed(pe)
	if !ok || msg != "claude process not running" {
		t.Fatalf("wire round-trip AgentFailed = %q, %v", msg, ok)
	}
}

// The 🎯T623 specimen: a string that quotes a refusal must not look like one.
func TestAgentFailedQuotedRefusalIsNotTyped(t *testing.T) {
	t.Parallel()
	quoted := errors.New("provider response quoted: " + (&broker.ProtocolError{
		Code: broker.CodeAgentFailed, Msg: "grok acp: prompt already in flight",
	}).Error())
	if msg, ok := AgentFailed(quoted); ok {
		t.Fatalf("quoted Error() text matched: %q", msg)
	}
}
