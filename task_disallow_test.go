// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// argvFor rebuilds the argv the claude backend passes, so these tests
// assert on what is actually handed to the process rather than on what
// the caller intended. Intent is precisely what failed here: a consumer
// moved from Session mode to Task mode, kept passing DisallowTools in
// spirit, and silently lost every restriction because Task mode ignored
// the concept entirely.
func argvFor(req taskRunRequest) []string {
	args := []string{
		"-p", "--verbose",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
		"--disallowedTools", disallowedToolList(req.DisallowTools),
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return append(args, req.Prompt)
}

func disallowedIn(t *testing.T, argv []string) string {
	t.Helper()
	i := slices.Index(argv, "--disallowedTools")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv carries no --disallowedTools: %v", argv)
	}
	return argv[i+1]
}

// TestTaskAlwaysDisallowsAgentSpawning is the guarantee the package
// documents and Task mode did not keep.
//
// A task that can spawn sub-agents takes process-lifecycle ownership away
// from the host, and turns any imperative sentence in its input into a
// fan-out. That is not hypothetical: a summariser handed a transcript
// containing "go deep with fanout" obeyed it and produced roughly 33,000
// subagents and 4.3 billion tokens in two hours.
func TestTaskAlwaysDisallowsAgentSpawning(t *testing.T) {
	got := disallowedIn(t, argvFor(taskRunRequest{Prompt: "summarise this"}))
	for _, tool := range strings.Split(BaseDisallowedTools, ",") {
		if !slices.Contains(strings.Split(got, ","), tool) {
			t.Errorf("%q missing from --disallowedTools %q", tool, got)
		}
	}
}

// TestTaskHonoursCallerDisallowTools: a summariser must be able to remove
// shell, filesystem and network tools. Before this existed there was no
// way to express that in Task mode at all.
func TestTaskHonoursCallerDisallowTools(t *testing.T) {
	extra := []string{"Bash", "Edit", "Write", "WebFetch", "WebSearch"}
	got := disallowedIn(t, argvFor(taskRunRequest{
		Prompt: "summarise this", DisallowTools: extra,
	}))
	list := strings.Split(got, ",")
	for _, tool := range extra {
		if !slices.Contains(list, tool) {
			t.Errorf("caller-supplied %q missing from %q", tool, got)
		}
	}
	// The baseline must survive alongside the caller's additions rather
	// than being replaced by them.
	if !slices.Contains(list, "Agent") {
		t.Errorf("caller's list displaced the baseline: %q", got)
	}
}

// TestTaskDisallowFlagPrecedesPrompt guards an ordering mistake that
// would silently disarm the flag: the prompt is positional and anything
// after it is consumed as prompt text, not as an option.
func TestTaskDisallowFlagPrecedesPrompt(t *testing.T) {
	argv := argvFor(taskRunRequest{Prompt: "the prompt"})
	flagAt := slices.Index(argv, "--disallowedTools")
	promptAt := slices.Index(argv, "the prompt")
	if flagAt < 0 || promptAt < 0 {
		t.Fatalf("unexpected argv: %v", argv)
	}
	if flagAt > promptAt {
		t.Errorf("--disallowedTools at %d comes after the prompt at %d, so it would be "+
			"swallowed as prompt text: %v", flagAt, promptAt, argv)
	}
}

// TestSessionAndTaskShareTheBaseline: the two modes must not drift. The
// original bug was exactly this divergence — the guarantee was
// implemented once, in one mode, and documented as universal.
func TestSessionAndTaskShareTheBaseline(t *testing.T) {
	if got := disallowedToolList(nil); got != BaseDisallowedTools {
		t.Errorf("disallowedToolList(nil) = %q, want the shared baseline %q",
			got, BaseDisallowedTools)
	}
}

// TestCodexTaskToolRestrictionsFailClosed (🎯T4.6) is the same failure
// mode as the tests above, one provider over. `codex exec` has no
// per-tool disallow flag, so honouring DisallowTools is impossible —
// and running anyway would hand the caller a fully-armed agent while
// they believe shell and filesystem access were removed. The run must
// be refused with a typed error before any process is spawned.
//
// This test is hermetic on purpose: no CODEX_BIN, no auth. If the guard
// is removed the run reaches the auth preflight and fails with a
// different, untyped error, so the assertion is on the type, not merely
// on failure.
func TestCodexTaskToolRestrictionsFailClosed(t *testing.T) {
	task := NewTask(TaskConfig{
		Provider:      ProviderCodex,
		ID:            "codex-restricted",
		WorkDir:       t.TempDir(),
		DisallowTools: []string{"Bash", "Write", "WebFetch"},
	})
	_, err := task.Run(context.Background(), "summarise this untrusted text")
	if err == nil {
		t.Fatal("Codex Task.Run honoured DisallowTools silently; want a capability error")
	}
	var capErr *CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %T %v, want *CapabilityError", err, err)
	}
	if capErr.Provider != ProviderCodex ||
		capErr.Capability != CapabilityToolRestrictions ||
		capErr.Status != CapabilityUnsupported {
		t.Errorf("CapabilityError = %+v", capErr)
	}
	if got := task.Status(); got != TaskStatusError {
		t.Errorf("task status = %q, want %q", got, TaskStatusError)
	}
}

// TestCodexTaskWithoutRestrictionsIsNotBlocked keeps the guard narrow:
// refusing every Codex task would pass the test above for the wrong
// reason. A task with no DisallowTools must get past the capability gate
// and fail later, on binary/auth resolution.
func TestCodexTaskWithoutRestrictionsIsNotBlocked(t *testing.T) {
	task := NewTask(TaskConfig{
		Provider: ProviderCodex,
		ID:       "codex-unrestricted",
		WorkDir:  t.TempDir(),
	})
	_, err := task.Run(context.Background(), "summarise this")
	var capErr *CapabilityError
	if errors.As(err, &capErr) && capErr.Capability == CapabilityToolRestrictions {
		t.Fatalf("unrestricted Codex task blocked by the tool-restriction gate: %v", err)
	}
}

// TestGrokTaskToolRestrictionsFailClosed (🎯T23) is the Codex guard above
// carried to the provider that still had the hole: grokTaskArgs built
// argv from prompt, output-format, permission-mode, cwd, model and
// resume, and never looked at req.DisallowTools. A caller who removed
// shell, filesystem and network tools from a summariser was handed all
// three back with no signal.
//
// GROK_BIN deliberately points at a fake CLI that succeeds. A test that
// leaned on grok being absent would pass for the wrong reason on the
// only hosts that matter — the ones with grok installed — and would stay
// green with the guard deleted. Here, deleting the guard lets the run
// reach the fake, return no error, and fail the assertion below.
func TestGrokTaskToolRestrictionsFailClosed(t *testing.T) {
	t.Setenv("GROK_BIN", writeFakeCLI(t, "testdata/grok/exec/success.jsonl", 0))

	task := NewTask(TaskConfig{
		Provider:      ProviderGrok,
		ID:            "grok-restricted",
		WorkDir:       t.TempDir(),
		DisallowTools: []string{"Bash", "Write", "WebFetch"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := task.Run(ctx, "summarise this untrusted text")
	if err == nil {
		t.Fatal("Grok Task.Run dropped DisallowTools and spawned anyway; want a capability error")
	}
	var capErr *CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %T %v, want *CapabilityError", err, err)
	}
	if capErr.Provider != ProviderGrok ||
		capErr.Capability != CapabilityToolRestrictions ||
		capErr.Status != CapabilityUnsupported {
		t.Errorf("CapabilityError = %+v", capErr)
	}
	if got := task.Status(); got != TaskStatusError {
		t.Errorf("task status = %q, want %q", got, TaskStatusError)
	}
}

// TestGrokTaskWithoutRestrictionsIsNotBlocked keeps that guard narrow.
// Refusing every Grok task would satisfy the test above for entirely the
// wrong reason, so an unrestricted task must not merely be allowed to
// start — it must run to a result event through the real backend.
func TestGrokTaskWithoutRestrictionsIsNotBlocked(t *testing.T) {
	t.Setenv("GROK_BIN", writeFakeCLI(t, "testdata/grok/exec/success.jsonl", 0))

	task := NewTask(TaskConfig{
		Provider: ProviderGrok,
		ID:       "grok-unrestricted",
		WorkDir:  t.TempDir(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events, err := task.Run(ctx, "summarise this")
	if err != nil {
		t.Fatalf("unrestricted Grok task refused: %v", err)
	}
	var sawResult bool
	for _, ev := range drainTaskEvents(t, events) {
		if ev.Type == TaskEventResult {
			sawResult = true
		}
	}
	if !sawResult {
		t.Error("unrestricted Grok task produced no result event; the guard is over-broad")
	}
}

// TestGrokTaskArgsCarryNoToolRestrictionFlags pins the refusal as the
// whole implementation. grok really does understand `--deny Bash` and
// `--disallowed-tools run_terminal_cmd`, so the tempting half-fix is to
// emit one and call the gap closed — which would restore the silent drop
// for every name the translation got wrong, under a green claim. The
// guard, the argv builder and the published claim have to move together;
// whoever wires the translation replaces all three, and this test with
// them.
func TestGrokTaskArgsCarryNoToolRestrictionFlags(t *testing.T) {
	argv := grokTaskArgs(taskRunRequest{
		Prompt:        "the prompt",
		DisallowTools: []string{"Bash"},
	})
	for _, arg := range argv {
		lower := strings.ToLower(arg)
		if strings.Contains(lower, "deny") || strings.Contains(lower, "disallow") || lower == "bash" {
			t.Errorf("grokTaskArgs emitted an untested tool-restriction argument %q: %v", arg, argv)
		}
	}
}

// TestGrokToolRestrictionRefusalSurvivesASupportedClaim covers the
// branch that only fires after someone else's edit: flipping the Grok
// tool_restrictions claim to supported makes CheckCapability return nil,
// and a backend that returned that nil would answer (nil, nil) — no run,
// no error, and a caller holding the armed agent they asked not to have.
// Asserted through the helper rather than by swapping the claim map,
// which twenty parallel tests read concurrently.
func TestGrokToolRestrictionRefusalSurvivesASupportedClaim(t *testing.T) {
	err := grokToolRestrictionRefusal(nil)
	if err == nil {
		t.Fatal("a supported claim collapsed the refusal to nil; Task.Run would return (nil, nil)")
	}
	var capErr *CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %T %v, want *CapabilityError", err, err)
	}
	if capErr.Capability != CapabilityToolRestrictions {
		t.Errorf("CapabilityError = %+v, want tool_restrictions", capErr)
	}
	// The reason must point at the unwired argv builder, not recite the
	// ordinary gap: the whole point is that the claim moved and the code
	// did not.
	if !strings.Contains(capErr.Reason, "grokTaskArgs") {
		t.Errorf("reason does not name the code that must change: %q", capErr.Reason)
	}
	// The ordinary path must still surface the ordinary reason.
	if got := grokToolRestrictionRefusal(
		CheckCapability(ProviderGrok, CapabilityToolRestrictions)); got == nil ||
		strings.Contains(got.Error(), "grokTaskArgs") {
		t.Errorf("refusal under the real claim = %v, want the published rationale", got)
	}
}

// TestCodexTaskArgsCarryNoToolRestrictionFlags closes the other escape
// hatch: "fixing" the gap by forging a Claude flag Codex does not
// understand would restore the silent failure while looking green.
func TestCodexTaskArgsCarryNoToolRestrictionFlags(t *testing.T) {
	argv := codexTaskArgs(taskRunRequest{
		Prompt:        "the prompt",
		DisallowTools: []string{"Bash"},
	})
	for _, arg := range argv {
		lower := strings.ToLower(arg)
		if strings.Contains(lower, "disallow") || strings.Contains(lower, "bash") {
			t.Errorf("codexTaskArgs forged a tool-restriction argument %q: %v", arg, argv)
		}
	}
}
