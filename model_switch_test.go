// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSetModelEmptyRejected(t *testing.T) {
	a := &Agent{
		provider: ProviderClaude,
		alive:    true,
		ready:    make(chan struct{}),
		ops:      agentOps{setModel: func(*Agent, string) error { return nil }},
	}
	close(a.ready)
	if err := a.SetModel("  "); err == nil {
		t.Fatal("expected error for empty model")
	}
}

func TestSetModelRefusesWhilePromptInFlight(t *testing.T) {
	a := &Agent{
		provider: ProviderClaude,
		alive:    true,
		ready:    make(chan struct{}),
		ops: agentOps{
			promptInFlight: func(*Agent) bool { return true },
			setModel:       func(*Agent, string) error { t.Fatal("setModel must not run"); return nil },
		},
	}
	close(a.ready)
	err := a.SetModel("sonnet")
	if err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetModelClaudePublishesSystemEventAndUpdatesModel(t *testing.T) {
	var sent []string
	a := &Agent{
		provider:  ProviderClaude,
		sessionID: "sess-model",
		alive:     true,
		ready:     make(chan struct{}),
		eventSubs: map[int64]EventFunc{},
		ops: agentOps{
			setModel: func(_ *Agent, model string) error {
				sent = append(sent, model)
				return nil
			},
		},
	}
	close(a.ready)

	var got []Event
	a.SubscribeEvents(func(ev Event) { got = append(got, ev) })

	if err := a.SetModel("haiku"); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "haiku" {
		t.Fatalf("sent = %v", sent)
	}
	if a.Model() != "haiku" {
		t.Fatalf("Model() = %q", a.Model())
	}
	if len(got) != 1 || got[0].Type != "system" || got[0].Model != "haiku" {
		t.Fatalf("events = %+v", got)
	}
}

func TestSetModelUnsupportedProvider(t *testing.T) {
	a := &Agent{
		provider: ProviderOllama,
		alive:    true,
		ready:    make(chan struct{}),
		ops:      agentOps{setModel: func(*Agent, string) error { return nil }},
	}
	close(a.ready)
	err := a.SetModel("llama3")
	ce, ok := err.(*CapabilityError)
	if !ok {
		t.Fatalf("err = %T %v, want *CapabilityError", err, err)
	}
	if ce.Capability != CapabilityModelSwitch {
		t.Fatalf("capability = %q", ce.Capability)
	}
}

func TestACPSetModelPrefersConfigOption(t *testing.T) {
	var methods []string
	request := func(method string, params any) (json.RawMessage, error) {
		methods = append(methods, method)
		if method == "session/set_config_option" {
			return json.RawMessage(`{}`), nil
		}
		return nil, errors.New("method not found")
	}
	if err := acpSetModel(request, "sid", "grok-4"); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || methods[0] != "session/set_config_option" {
		t.Fatalf("methods = %v", methods)
	}
}

func TestACPSetModelFallsBackToLegacySetModel(t *testing.T) {
	var methods []string
	request := func(method string, params any) (json.RawMessage, error) {
		methods = append(methods, method)
		if method == "session/set_model" {
			return json.RawMessage(`{}`), nil
		}
		return nil, errors.New("method not found")
	}
	if err := acpSetModel(request, "sid", "composer-2"); err != nil {
		t.Fatal(err)
	}
	if methods[len(methods)-1] != "session/set_model" {
		t.Fatalf("methods = %v", methods)
	}
}

func TestCodexSetModelUpdatesClientModel(t *testing.T) {
	c := &codexAppServerClient{model: "gpt-old"}
	if err := c.SetModel("gpt-new"); err != nil {
		t.Fatal(err)
	}
	if c.Model() != "gpt-new" {
		t.Fatalf("Model = %q", c.Model())
	}
}
