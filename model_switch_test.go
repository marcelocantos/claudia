// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSwitchModelEmptyRejected(t *testing.T) {
	a := &Agent{
		provider: ProviderClaude,
		alive:    true,
		ready:    make(chan struct{}),
		ops:      agentOps{switchModel: func(*Agent, Provider, string) error { return nil }},
	}
	close(a.ready)
	if err := a.SwitchModel("", "  "); err == nil {
		t.Fatal("expected error for empty model")
	}
}

func TestSwitchModelRefusesWhilePromptInFlight(t *testing.T) {
	a := &Agent{
		provider: ProviderClaude,
		alive:    true,
		ready:    make(chan struct{}),
		ops: agentOps{
			promptInFlight: func(*Agent) bool { return true },
			switchModel:    func(*Agent, Provider, string) error { t.Fatal("switchModel must not run"); return nil },
		},
	}
	close(a.ready)
	err := a.SwitchModel("", "sonnet")
	if err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("err = %v", err)
	}
}

// 🎯T195: a switch publishes one model_switch event naming both ends, updates
// the handle, and runs the registry's record hook so the definition follows.
func TestSwitchModelPublishesSwitchEventAndRecords(t *testing.T) {
	type call struct {
		p Provider
		m string
	}
	var sent []call
	recorded := 0
	a := &Agent{
		provider:  "anthropic",
		model:     "claude-sonnet-5",
		sessionID: "sess-model",
		alive:     true,
		ready:     make(chan struct{}),
		eventSubs: map[int64]EventFunc{},
		ops: agentOps{
			switchModel: func(_ *Agent, p Provider, model string) error {
				sent = append(sent, call{p, model})
				return nil
			},
		},
	}
	a.onSwitched = func() error { recorded++; return nil }
	close(a.ready)

	var got []Event
	a.SubscribeEvents(func(ev Event) { got = append(got, ev) })

	if err := a.SwitchModel("openai-codex", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != (call{"openai-codex", "gpt-5.5"}) {
		t.Fatalf("sent = %v", sent)
	}
	if a.Model() != "gpt-5.5" || a.Provider() != "openai-codex" {
		t.Fatalf("handle = %s/%s", a.Provider(), a.Model())
	}
	if recorded != 1 {
		t.Fatalf("record hook ran %d times, want 1", recorded)
	}
	if len(got) != 1 || got[0].ProgressType != ProgressModelSwitch || got[0].FromProvider != "anthropic" ||
		got[0].ToProvider != "openai-codex" || got[0].FromModel != "claude-sonnet-5" || got[0].Model != "gpt-5.5" {
		t.Fatalf("events = %+v", got)
	}
	if got[0].RequestID != "" || got[0].Receipt != nil {
		t.Fatalf("switch carried a request receipt: %+v", got[0])
	}
}

// A native CLI backend changes model only within its provider.
func TestSwitchModelNativeBackendRefusesProviderChange(t *testing.T) {
	a := &Agent{provider: ProviderCodex, alive: true, ready: make(chan struct{})}
	a.ops.switchModel = func(a *Agent, p Provider, _ string) error { return sameProviderSwitch(a, p) }
	close(a.ready)
	err := a.SwitchModel(ProviderClaude, "sonnet")
	var ce *CapabilityError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "only a sidecar seat changes provider") {
		t.Fatalf("err = %v, want a CapabilityError naming the sidecar", err)
	}
	if a.Provider() != ProviderCodex {
		t.Fatalf("provider = %s after a refused switch", a.Provider())
	}
}

func TestSwitchModelUnsupportedProvider(t *testing.T) {
	a := &Agent{
		provider: ProviderOllama,
		alive:    true,
		ready:    make(chan struct{}),
		ops:      agentOps{switchModel: func(*Agent, Provider, string) error { return nil }},
	}
	close(a.ready)
	err := a.SwitchModel("", "llama3")
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
