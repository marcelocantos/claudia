// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDistillInertSeedRefusesEmpty(t *testing.T) {
	got := distillInertSeed(nil, "ship T55", "claude")
	if !got.empty {
		t.Fatal("empty turns must be empty seed")
	}
	if got.LastUser != "" || got.LastAssistant != "" {
		t.Fatalf("last = %q / %q", got.LastUser, got.LastAssistant)
	}
}

func TestDistillInertSeedOmitsExecutableToolCalls(t *testing.T) {
	got := distillInertSeed([]inertTurn{
		{Role: "user", Text: "patch migrate.go and run make gate"},
		{Role: "assistant", Text: "editing migrate.go", ToolNames: []string{"Bash"}},
	}, "", "claude")
	if got.empty {
		t.Fatal("seed should not be empty")
	}
	if got.LastUser == "" || got.LastAssistant == "" {
		t.Fatalf("last user/assistant empty: %+v", got)
	}
	if got.LastAssistant != "editing migrate.go" {
		t.Fatalf("last assistant = %q", got.LastAssistant)
	}
	if !strings.Contains(got.Text, "Inert foreign tools") || !strings.Contains(got.Text, "Bash") {
		t.Fatalf("seed missing inert tool names:\n%s", got.Text)
	}
	if strings.Contains(got.Text, `"type":"tool_use"`) || strings.Contains(strings.ToLower(got.Text), "invoke bash") {
		t.Fatalf("seed contains executable tool call:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "INERT PREDECESSOR") {
		t.Fatalf("seed missing inert header:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "migrate.go") {
		t.Fatalf("seed missing file mention:\n%s", got.Text)
	}
	if len(got.WarningCodes) == 0 || got.WarningCodes[0] != "stale_tool_output" {
		t.Fatalf("warnings = %v", got.WarningCodes)
	}
}

func TestClassifyStuckEvent(t *testing.T) {
	class, _ := classifyStuckEvent(Event{IsError: true, Text: "rate_limit exceeded"})
	if class != StuckClassRateLimit {
		t.Fatalf("rate_limit class = %q", class)
	}
	class, _ = classifyStuckEvent(Event{IsError: true, Text: "weekly quota exhausted"})
	if class != StuckClassQuota {
		t.Fatalf("quota class = %q", class)
	}
	if class, _ = classifyStuckEvent(Event{IsError: true, Text: "unauthorized"}); class != "" {
		t.Fatalf("auth classified as %q", class)
	}
	if class, _ = classifyStuckEvent(Event{IsError: true, Text: "model_not_found"}); class != "" {
		t.Fatalf("model_not_found classified as %q", class)
	}
	if class, _ = classifyStuckEvent(Event{Text: "rate_limit"}); class != "" {
		t.Fatal("non-error rate_limit text must not classify")
	}
}

func startMigrateFixture(t *testing.T, provider Provider, name string) (*Agent, *fakeAgentBackend) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))
	backend := &fakeAgentBackend{name: name}
	agent, err := startWithBackend(Config{
		Provider:  provider,
		WorkDir:   t.TempDir(),
		SessionID: name + "-session",
		Model:     "src-model",
		Goal:      "land T55 migrate",
	}, backend)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(agent.Stop)
	return agent, backend
}

func TestMigrateSuccessfulPairKeepsHandleAndSubscription(t *testing.T) {
	src, srcBack := startMigrateFixture(t, ProviderClaude, "fake-claude")
	dest := &fakeAgentBackend{name: "fake-grok", assignedSession: "grok-dest-1"}

	var got []Event
	src.SubscribeEvents(func(ev Event) { got = append(got, ev) })

	src.PublishEvent(Event{Type: "user", Text: "patch migrate.go"})
	src.PublishEvent(Event{Type: "assistant", Text: "working on migrate.go"})
	src.PublishEvent(Event{Type: "progress", ProgressType: ProgressToolUse, ToolTitle: "Bash"})

	handle := src
	if err := src.migrateWithBackend(&MigrateArgs{
		Provider: ProviderGrok,
		Model:    "grok-4",
		Reason:   "explicit",
	}, dest); err != nil {
		t.Fatal(err)
	}
	if src != handle {
		t.Fatal("Migrate returned a different Agent handle")
	}
	if src.Provider() != ProviderGrok {
		t.Fatalf("provider = %q", src.Provider())
	}
	if src.SessionID() != "grok-dest-1" {
		t.Fatalf("session = %q, want rotated dest id", src.SessionID())
	}
	if src.JSONLPath() != "" {
		t.Fatalf("JSONLPath = %q, want empty on grok dest", src.JSONLPath())
	}

	dest.mu.Lock()
	reqs := dest.requests
	sends := append([]string(nil), dest.sends...)
	dest.mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("dest starts = %d", len(reqs))
	}
	if reqs[0].Resuming || reqs[0].SessionID != "" {
		t.Fatalf("dest must be a new native session, got resuming=%v id=%q", reqs[0].Resuming, reqs[0].SessionID)
	}
	if reqs[0].Config.RequireResume {
		t.Fatal("dest RequireResume must be false")
	}
	if len(sends) == 0 {
		t.Fatal("dest received no seed")
	}
	seed := sends[len(sends)-1]
	if !strings.Contains(seed, "INERT PREDECESSOR") {
		t.Fatalf("seed missing header: %s", seed)
	}
	if strings.Contains(seed, `"type":"tool_use"`) {
		t.Fatalf("seed contains executable tool call: %s", seed)
	}
	if !strings.Contains(seed, "Inert foreign tools") || !strings.Contains(seed, "Bash") {
		t.Fatalf("seed dropped tool name: %s", seed)
	}

	var switchEv *Event
	for i := range got {
		if got[i].ProgressType == ProgressModelSwitch {
			switchEv = &got[i]
			break
		}
	}
	if switchEv == nil {
		t.Fatalf("no model_switch event in %v", eventTypes(got))
	}
	if switchEv.FromProvider != ProviderClaude || switchEv.ToProvider != ProviderGrok {
		t.Fatalf("switch providers %+v", switchEv)
	}
	if switchEv.SessionID != "grok-dest-1" || switchEv.Reason != "explicit" {
		t.Fatalf("switch session/reason %+v", switchEv)
	}
	if switchEv.FromModel != "src-model" || switchEv.Model != "grok-4" {
		t.Fatalf("switch models from=%q to=%q", switchEv.FromModel, switchEv.Model)
	}

	n := len(got)
	src.PublishEvent(Event{Type: "assistant", Text: "dest reply", StopReason: "end_turn"})
	if len(got) <= n {
		t.Fatal("existing subscriber missed destination assistant event")
	}
	if got[len(got)-1].Text != "dest reply" {
		t.Fatalf("last event = %+v", got[len(got)-1])
	}
	if srcBack.stops == 0 {
		t.Fatal("source backend was not stopped")
	}
}

func TestMigrateRefusedPair(t *testing.T) {
	src, _ := startMigrateFixture(t, ProviderClaude, "fake-claude")
	src.PublishEvent(Event{Type: "user", Text: "hello"})
	err := src.Migrate(&MigrateArgs{Provider: ProviderOllama})
	var capErr *CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %T %v, want *CapabilityError", err, err)
	}
	if capErr.Capability != CapabilityMigrate || capErr.Provider != ProviderOllama {
		t.Fatalf("capErr = %+v", capErr)
	}
}

func TestMigrateEmptyJSONLPathStillSeedsFromEvents(t *testing.T) {
	src, _ := startMigrateFixture(t, ProviderGrok, "fake-grok")
	src.jsonlPath = ""
	if src.JSONLPath() != "" {
		t.Fatal("setup: JSONLPath not empty")
	}
	src.PublishEvent(Event{Type: "user", Text: "continue from events"})
	src.PublishEvent(Event{Type: "assistant", Text: "seeded from events"})
	dest := &fakeAgentBackend{name: "fake-claude", assignedSession: "claude-dest-1"}
	if err := src.migrateWithBackend(&MigrateArgs{Provider: ProviderClaude}, dest); err != nil {
		t.Fatal(err)
	}
	dest.mu.Lock()
	sends := append([]string(nil), dest.sends...)
	dest.mu.Unlock()
	if len(sends) == 0 {
		t.Fatal("no seed")
	}
	if !strings.Contains(sends[0], "continue from events") {
		t.Fatalf("seed missing last user: %s", sends[0])
	}
	if !strings.Contains(sends[0], "seeded from events") {
		t.Fatalf("seed missing last assistant: %s", sends[0])
	}
}

func TestMigrateMissingContextRefusesWithoutForce(t *testing.T) {
	src, _ := startMigrateFixture(t, ProviderClaude, "fake-claude")
	dest := &fakeAgentBackend{name: "fake-grok", assignedSession: "should-not-start"}
	err := src.migrateWithBackend(&MigrateArgs{Provider: ProviderGrok}, dest)
	if err == nil || !strings.Contains(err.Error(), "missing predecessor context") {
		t.Fatalf("err = %v", err)
	}
	dest.mu.Lock()
	n := len(dest.requests)
	dest.mu.Unlock()
	if n != 0 {
		t.Fatalf("dest started %d times on refuse", n)
	}
}

func TestMigrateForceColdStarts(t *testing.T) {
	src, _ := startMigrateFixture(t, ProviderClaude, "fake-claude")
	dest := &fakeAgentBackend{name: "fake-grok", assignedSession: "grok-cold"}
	if err := src.migrateWithBackend(&MigrateArgs{Provider: ProviderGrok, Force: true}, dest); err != nil {
		t.Fatal(err)
	}
	dest.mu.Lock()
	sends := append([]string(nil), dest.sends...)
	dest.mu.Unlock()
	if len(sends) == 0 || !strings.Contains(sends[0], "no distillable turns") {
		t.Fatalf("cold seed = %v", sends)
	}
}

func TestStuckExhaustedEventDoesNotMigrate(t *testing.T) {
	src, _ := startMigrateFixture(t, ProviderClaude, "fake-claude")
	var got []Event
	src.SubscribeEvents(func(ev Event) { got = append(got, ev) })
	src.PublishEvent(Event{Type: "assistant", Text: "rate_limit exceeded", IsError: true})
	var stuck *Event
	for i := range got {
		if got[i].ProgressType == ProgressStuck {
			stuck = &got[i]
			break
		}
	}
	if stuck == nil {
		t.Fatalf("no stuck event in %v", eventTypes(got))
	}
	if stuck.StuckClass != StuckClassRateLimit {
		t.Fatalf("StuckClass = %q", stuck.StuckClass)
	}
	if src.Provider() != ProviderClaude {
		t.Fatalf("provider changed to %q", src.Provider())
	}
}

func TestMigrateSameProviderRefused(t *testing.T) {
	src, _ := startMigrateFixture(t, ProviderClaude, "fake-claude")
	src.PublishEvent(Event{Type: "user", Text: "x"})
	err := src.Migrate(&MigrateArgs{Provider: ProviderClaude})
	if err == nil || !strings.Contains(err.Error(), "SetModel") {
		t.Fatalf("err = %v", err)
	}
}

func TestMigrateClaudeShapedDestKeepsMintedSessionID(t *testing.T) {
	src, _ := startMigrateFixture(t, ProviderGrok, "fake-grok")
	src.PublishEvent(Event{Type: "user", Text: "switch to claude"})
	src.PublishEvent(Event{Type: "assistant", Text: "ok"})
	fromID := src.SessionID()
	dest := &fakeAgentBackend{name: "fake-claude-attach", tailJSONL: true, omitStartIDs: true}

	var got []Event
	src.SubscribeEvents(func(ev Event) { got = append(got, ev) })

	if err := src.migrateWithBackend(&MigrateArgs{Provider: ProviderClaude, Model: "sonnet"}, dest); err != nil {
		t.Fatal(err)
	}
	dest.mu.Lock()
	if len(dest.requests) != 1 {
		dest.mu.Unlock()
		t.Fatal("dest not started")
	}
	reqID := dest.requests[0].SessionID
	workDir := dest.requests[0].WorkDir
	dest.mu.Unlock()
	if reqID == "" {
		t.Fatal("claude dest request SessionID is empty; mint never reached StartAgent")
	}
	if reqID == fromID {
		t.Fatal("dest SessionID reused the predecessor id")
	}
	if src.SessionID() != reqID {
		t.Fatalf("Agent.SessionID() = %q, want minted destID %q", src.SessionID(), reqID)
	}
	wantJSONL := SessionJSONLPath(reqID, workDir)
	if src.JSONLPath() != wantJSONL {
		t.Fatalf("JSONLPath = %q, want %q", src.JSONLPath(), wantJSONL)
	}
	var switchEv *Event
	for i := range got {
		if got[i].ProgressType == ProgressModelSwitch {
			switchEv = &got[i]
			break
		}
	}
	if switchEv == nil {
		t.Fatalf("no model_switch in %v", eventTypes(got))
	}
	if switchEv.SessionID != reqID {
		t.Fatalf("switch Event SessionID = %q, want %q", switchEv.SessionID, reqID)
	}
}

func TestMigrateSourceACPCloseLeavesDestAlive(t *testing.T) {
	src, _ := startMigrateFixture(t, ProviderGrok, "fake-grok")
	src.PublishEvent(Event{Type: "user", Text: "go"})
	src.PublishEvent(Event{Type: "assistant", Text: "ok"})
	var srcBind acpBind
	srcBind.attach(src)
	dest := &fakeAgentBackend{name: "fake-claude-attach", tailJSONL: true, omitStartIDs: true}
	if err := src.migrateWithBackend(&MigrateArgs{Provider: ProviderClaude}, dest); err != nil {
		t.Fatal(err)
	}
	srcBind.onClose()
	if !src.Alive() {
		t.Fatal("source ACP onClose after swap marked dest dead")
	}
}

func TestACPBindOnCloseIgnoresStaleGeneration(t *testing.T) {
	a := &Agent{alive: true}
	var b acpBind
	b.attach(a)
	a.backendGen.Add(1)
	b.onClose()
	if !a.Alive() {
		t.Fatal("source onClose after migrate marked dest dead")
	}
	b.attach(a)
	b.onClose()
	if a.Alive() {
		t.Fatal("current-generation onClose must mark the agent dead")
	}
}

func TestMigrateUnsupportedSource(t *testing.T) {
	a := &Agent{provider: ProviderOllama, alive: true, ready: make(chan struct{})}
	close(a.ready)
	err := a.Migrate(&MigrateArgs{Provider: ProviderGrok})
	var capErr *CapabilityError
	if !errors.As(err, &capErr) || capErr.Capability != CapabilityMigrate {
		t.Fatalf("err = %T %v", err, err)
	}
}

func eventTypes(evs []Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		if ev.ProgressType != "" {
			out[i] = ev.Type + "/" + ev.ProgressType
		} else {
			out[i] = ev.Type
		}
	}
	return out
}
