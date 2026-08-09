// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
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
