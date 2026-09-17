// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"reflect"
	"strings"
	"testing"
)

// The daemon itself and its suite live in package daemon (🎯T75.1). What
// stays here tests root code: the library's fallback when a socket has no
// daemon behind it, and the wire codecs for claudia types.

// TestBrokerDaemonBareServerFallsThroughToDirect: a protocol server with no
// daemon runtime answers not_available and the library takes the direct
// path (the 🎯T2.1 consult, preserved).
func TestBrokerDaemonBareServerFallsThroughToDirect(t *testing.T) {
	srv := startLibraryBroker(t)
	backend := &fakeAgentBackend{name: "fake-claude"}
	a, err := startConsideringBroker(Config{WorkDir: t.TempDir(), SessionID: "bare", TermLogPath: "-"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	if a.brokerGrant != "" {
		t.Fatal("bare server produced a broker-held handle")
	}
	if srv.RequestCount() == 0 {
		t.Fatal("bare server was never consulted")
	}
	backend.request(t)
	if BrokerAvailable() {
		t.Fatal("BrokerAvailable true against a bare protocol server")
	}
}

// TestBrokerWireMirrorsAreComplete is the 🎯T24 rule applied to the socket:
// every Config field either rides on AgentDef or is named as deliberately
// local; every ModelPredicates field rides or is named. Event, TaskEvent,
// TaskConfig and ModelPick are pinned by struct conversion at compile time.
func TestBrokerWireMirrorsAreComplete(t *testing.T) {
	defFields := map[string]bool{}
	for f := range reflect.TypeFor[AgentDef]().Fields() {
		defFields[f.Name] = true
	}
	for f := range reflect.TypeFor[Config]().Fields() {
		if defFields[f.Name] || f.Name == "RequireResume" {
			continue
		}
		_, local := configNotOnGrantWire[f.Name]
		_, callback := configByCallback[f.Name]
		if !local && !callback {
			t.Errorf("Config.%s is neither on AgentDef (the grant wire), declared local in configNotOnGrantWire, nor honoured by callback in configByCallback", f.Name)
		}
		if local && callback {
			t.Errorf("Config.%s is declared both not carried and honoured by callback", f.Name)
		}
	}
	for _, declared := range []map[string]string{configNotOnGrantWire, configByCallback} {
		for name := range declared {
			if _, ok := reflect.TypeFor[Config]().FieldByName(name); !ok {
				t.Errorf("%s is declared off the grant wire, but Config no longer has it", name)
			}
		}
	}
	predFields := map[string]bool{}
	for f := range reflect.TypeFor[predicatesWire]().Fields() {
		predFields[f.Name] = true
	}
	for f := range reflect.TypeFor[ModelPredicates]().Fields() {
		if predFields[f.Name] {
			continue
		}
		if _, ok := predicatesNotOnWire[f.Name]; !ok {
			t.Errorf("ModelPredicates.%s is not on predicatesWire and not declared daemon-supplied", f.Name)
		}
	}
	// Wire codecs round-trip.
	ev := Event{Type: "assistant", Text: "x", Raw: []byte(`{"a":1}`), Usage: Usage{InputTokens: 1},
		WarningCodes: []string{"w"}, StuckClass: StuckClassQuota, FromProvider: ProviderGrok}
	raw, err := EncodeEventWire(ev)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeEventWire(raw)
	if err != nil || !reflect.DeepEqual(back, ev) {
		t.Fatalf("event round trip: %+v vs %+v (%v)", back, ev, err)
	}
	if strings.Contains(string(raw), `"turn_id"`) {
		t.Fatalf("empty fields must be omitted on the wire: %s", raw)
	}
}

func TestDecodePredicatesWireSkillAlias(t *testing.T) {
	got, err := DecodePredicatesWire([]byte(`{"skill":"analysis","quality":"standard"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Purpose != ModelPurposeAnalysis || got.Skill != ModelPurposeAnalysis {
		t.Fatalf("skill alias: %+v", got)
	}
	raw, err := EncodePredicatesWire(ModelPredicates{Skill: ModelPurposeAnalysis})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"purpose":"analysis"`) {
		t.Fatalf("encode must materialize purpose from skill: %s", raw)
	}
}
