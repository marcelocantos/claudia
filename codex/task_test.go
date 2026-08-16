// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestExecArgs(t *testing.T) {
	got := execArgs(execArgInput{
		WorkDir:        "/repo",
		Model:          "gpt-5.4",
		SandboxMode:    "read-only",
		ApprovalPolicy: "never",
		Prompt:         "summarize",
	})
	want := []string{
		"--ask-for-approval", "never",
		"--cd", "/repo",
		"--sandbox", "read-only",
		"--model", "gpt-5.4",
		"exec",
		"--json",
		"summarize",
	}
	if !slicesEqual(got, want) {
		t.Errorf("execArgs = %v, want %v", got, want)
	}
}

func TestExecArgsResume(t *testing.T) {
	got := execArgs(execArgInput{SessionID: "thread-123", Prompt: "continue"})
	want := []string{"exec", "resume", "--json", "thread-123", "continue"}
	if !slicesEqual(got, want) {
		t.Errorf("resume = %v, want %v", got, want)
	}
}

func TestHermeticTaskRunSuccess(t *testing.T) {
	bin := writeFakeCLI(t, "testdata/exec/success.jsonl", 0)

	task := NewCodexTask(Config{
		ID:      "hermetic-success",
		WorkDir: t.TempDir(),
		Model:   "gpt-5.4",
		Resolve: hermeticResolve(t, bin),
	})

	// t.Context(): the deadline bounded process cleanup, but its expiry
	// produced a failing assertion — so under load the constant, not the
	// product, decided the verdict. t.Context() cleans up just as well and
	// cannot fire early; a genuine hang is `go test -timeout`'s job (🎯T31).
	ctx := t.Context()
	events, err := task.Run(ctx, "summarize")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drain(t, events)

	var sawInit, sawResult bool
	for _, ev := range got {
		switch ev.Type {
		case EventInit:
			sawInit = true
			if ev.SessionID == "" {
				t.Error("empty session id")
			}
		case EventResult:
			sawResult = true
			if ev.Content != "Final answer." {
				t.Errorf("result content = %q", ev.Content)
			}
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Error)
		}
	}
	if !sawInit || !sawResult {
		t.Fatalf("incomplete events init=%v result=%v got=%#v", sawInit, sawResult, got)
	}
	if task.SessionID() == "" {
		t.Error("SessionID empty after run")
	}
	if task.LastResult() != "Final answer." {
		t.Errorf("LastResult = %q", task.LastResult())
	}
	if task.Status() != StatusIdle {
		t.Errorf("status = %q, want idle", task.Status())
	}
}

func TestHermeticTaskRunRateLimit(t *testing.T) {
	bin := writeFakeCLI(t, "testdata/exec/rate_limit.jsonl", 0)
	task := NewTask(Config{
		Resolve: hermeticResolve(t, bin),
	})
	// t.Context(), not a deadline that can decide the verdict (🎯T31).
	ctx := t.Context()
	events, err := task.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drain(t, events)
	var rl *RateLimitError
	for _, ev := range got {
		if ev.Type == EventError && errors.As(ev.Error, &rl) {
			return
		}
	}
	t.Fatalf("events = %#v, want *RateLimitError", got)
}

func TestHermeticTaskRunAuthFailJSONL(t *testing.T) {
	bin := writeFakeCLI(t, "testdata/exec/auth_fail.jsonl", 0)
	task := NewTask(Config{
		Resolve: hermeticResolve(t, bin),
	})
	// t.Context(), not a deadline that can decide the verdict (🎯T31).
	ctx := t.Context()
	events, err := task.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drain(t, events)
	var ae *AuthError
	for _, ev := range got {
		if ev.Type == EventError && errors.As(ev.Error, &ae) {
			return
		}
	}
	t.Fatalf("events = %#v, want *AuthError", got)
}

func TestHermeticTaskRunNonZeroExit(t *testing.T) {
	// Empty stdout + exit 3 → ExitError when no structured error line.
	bin := writeFakeCLI(t, "testdata/exec/malformed.jsonl", 3)
	// malformed lines produce no events, so Wait non-zero should surface ExitError.
	// Use a truly empty fixture for cleaner signal:
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	bin = writeFakeCLIAbs(t, empty, 3)

	task := NewTask(Config{
		Resolve: hermeticResolve(t, bin),
	})
	// t.Context(), not a deadline that can decide the verdict (🎯T31).
	ctx := t.Context()
	events, err := task.Run(ctx, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := drain(t, events)
	var ee *ExitError
	for _, ev := range got {
		if ev.Type == EventError && errors.As(ev.Error, &ee) {
			if ee.ExitCode != 3 {
				t.Errorf("ExitCode = %d, want 3", ee.ExitCode)
			}
			return
		}
	}
	t.Fatalf("events = %#v, want *ExitError", got)
}

func TestHermeticTaskAuthPreflightFails(t *testing.T) {
	// Missing auth file → *AuthError from Run before spawn.
	missing := filepath.Join(t.TempDir(), "no-such-auth.json")
	task := NewTask(Config{
		Resolve: &ResolveArgs{
			BinPath:  writeFakeCLI(t, "testdata/exec/success.jsonl", 0),
			AuthPath: missing,
			Getenv: func(k string) string {
				if k == openaiKey {
					return ""
				}
				return ""
			},
		},
	})
	_, err := task.Run(context.Background(), "hi")
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("Run err = %v (%T), want *AuthError", err, err)
	}
}

func TestTaskStopBlocksRun(t *testing.T) {
	task := NewTask(Config{ID: "stop-me"})
	task.Stop()
	if task.Status() != StatusStopped {
		t.Fatalf("status = %q", task.Status())
	}
	_, err := task.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error on stopped task")
	}
}

func writeHermeticAuth(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	body := `{"auth_mode":"chatgpt","tokens":{"access_token":"hermetic-tok"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hermeticResolve returns ResolveArgs with fixture auth and no OPENAI_API_KEY
// fall-through, independent of the host environment.
func hermeticResolve(t *testing.T, bin string) *ResolveArgs {
	t.Helper()
	authPath := writeHermeticAuth(t)
	return &ResolveArgs{
		BinPath:  bin,
		AuthPath: authPath,
		Getenv: func(k string) string {
			if k == openaiKey {
				return ""
			}
			if k == authPathEnv {
				return authPath
			}
			return os.Getenv(k)
		},
	}
}

func writeFakeCLI(t *testing.T, fixturePath string, exitCode int) string {
	t.Helper()
	abs, err := filepath.Abs(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	return writeFakeCLIAbs(t, abs, exitCode)
}

func writeFakeCLIAbs(t *testing.T, absFixture string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hermetic fake CLI uses POSIX shell")
	}
	if _, err := os.Stat(absFixture); err != nil {
		t.Fatalf("fixture %s: %v", absFixture, err)
	}
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := "#!/bin/sh\n" +
		"cat \"" + absFixture + "\"\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func drain(t *testing.T, events <-chan Event) []Event {
	t.Helper()
	var got []Event
	for ev := range events {
		got = append(got, ev)
	}
	return got
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
