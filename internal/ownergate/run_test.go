// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ownergate

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, append([]string{"-c", "user.name=Gate test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, checks string) string {
	t.Helper()
	dir := t.TempDir()
	fixtureGit(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "Makefile"), "gate-checks:\n\t"+checks+"\n")
	write(t, filepath.Join(dir, "source.txt"), "committed source\n")
	write(t, filepath.Join(dir, ".gitignore"), "ignored/\n")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-qm", "fixture")
	return dir
}

func attestation(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".git", "gate-attestation"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func noAttestation(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git", "gate-attestation")); !os.IsNotExist(err) {
		t.Fatalf("failed gate left an attestation: %v", err)
	}
}

func TestSnapshotExcludesHiddenInputsAndWorkspace(t *testing.T) {
	dir := fixture(t, `test "$$GOWORK" = off && test "$$GOFLAGS" = -mod=readonly && test "$$CLAUDIA_NO_BROKER" = 1 && test -z "$$CLAUDIA_OMP_LIVE" && test ! -e ignored/poison.go`)
	if err := os.Mkdir(filepath.Join(dir, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "ignored", "poison.go"), "not compilable")
	t.Setenv("GOWORK", "/nonexistent/sibling/go.work")
	t.Setenv("GOFLAGS", "-run=NoTests")
	t.Setenv("CLAUDIA_OMP_LIVE", "1")
	if err := Run(dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := attestation(t, dir)
	for _, want := range []string{"profile=" + Profile, "dirty=0", "tree=" + fixtureGit(t, dir, "rev-parse", "HEAD^{tree}")} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}

func TestDirtyAndUntrackedStartCannotAttest(t *testing.T) {
	for _, name := range []string{"source.txt", "untracked.go"} {
		t.Run(name, func(t *testing.T) {
			dir := fixture(t, "true")
			write(t, filepath.Join(dir, ".git", "gate-attestation"), "stale pass")
			write(t, filepath.Join(dir, name), "uncommitted")
			err := Run(dir, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "uncommitted inputs") {
				t.Fatalf("got %v", err)
			}
			// Cleaning after refusal cannot revive a pending begin/end record.
			if name == "source.txt" {
				write(t, filepath.Join(dir, name), "committed source\n")
			}
			noAttestation(t, dir)
		})
	}
}

// At the first status line the clone exists and the real runner owns its lock.
type intercept struct {
	onStart func(string)
	buffer  bytes.Buffer
}

func (w *intercept) Write(p []byte) (int, error) {
	if w.onStart != nil {
		f := w.onStart
		w.onStart = nil
		f(string(p))
	}
	return w.buffer.Write(p)
}

func TestChangedInputsAndFailedChecksNeverAttest(t *testing.T) {
	for _, scenario := range []string{"dirty-end", "untracked-end", "new-commit", "snapshot-dirty", "failed-check"} {
		t.Run(scenario, func(t *testing.T) {
			checks := "true"
			if scenario == "snapshot-dirty" {
				checks = "touch unexpected.go"
			}
			if scenario == "failed-check" {
				checks = "false"
			}
			dir := fixture(t, checks)
			w := &intercept{onStart: func(string) {
				switch scenario {
				case "dirty-end", "new-commit":
					write(t, filepath.Join(dir, "source.txt"), "changed")
					if scenario == "new-commit" {
						fixtureGit(t, dir, "commit", "-qam", "changed")
					}
				case "untracked-end":
					write(t, filepath.Join(dir, "extra.go"), "changed")
				}
			}}
			if err := Run(dir, w); err == nil {
				t.Fatalf("%s passed: %s", scenario, &w.buffer)
			}
			noAttestation(t, dir)
		})
	}
}

func TestConcurrentRunCannotConsumeOrOverwriteAnotherRun(t *testing.T) {
	dir := fixture(t, "true")
	w := &intercept{onStart: func(string) {
		err := Run(dir, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "gate busy") {
			t.Fatalf("second run got %v", err)
		}
	}}
	if err := Run(dir, w); err != nil {
		t.Fatal(err)
	}
	attestation(t, dir)
}

func TestAmbientMakefileCannotSuppressChecks(t *testing.T) {
	dir := fixture(t, "exit 37")
	ambient := filepath.Join(t.TempDir(), "injected.mk")
	write(t, ambient, "SHELL := /usr/bin/true\n")
	t.Setenv("MAKEFILES", ambient)
	if err := Run(dir, io.Discard); err == nil {
		t.Fatal("external Makefile suppressed the failing checks")
	}
	noAttestation(t, dir)
}

func TestGitSelectionEnvironmentCannotRedirectSnapshot(t *testing.T) {
	dir := fixture(t, "true")
	foreign := fixture(t, "false")
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(foreign, ".git", "index"))
	if err := Run(dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	attestation(t, dir)
	noAttestation(t, foreign)
}

// Exercise the production Make recipe, real Bun, and real snapshot runner.
// A removed/unreachable sidecar command must not satisfy this negative oracle.
func TestProductionGateRejectsFailingSidecar(t *testing.T) {
	dir := fixture(t, "true")
	for _, name := range []string{"Makefile", "scripts/check-sidecar-prerequisites.ts", "sidecar/package.json", "sidecar/bun.lockb"} {
		b, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, ".gitignore"), "sidecar/node_modules/\n")
	const marker = "GATE_SIDECAR_PLANTED_FAILURE"
	write(t, filepath.Join(dir, "sidecar", "planted.test.ts"), "import { test, expect } from 'bun:test';\ntest('"+marker+"', () => expect(true).toBe(false));\n")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-qm", "plant sidecar failure")
	var output bytes.Buffer
	err := Run(dir, &output)
	if err == nil || !strings.Contains(output.String(), "(fail) "+marker) {
		t.Fatalf("planted test was not the failing check: %v\n%s", err, &output)
	}
	noAttestation(t, dir)
}

func TestAttestationPassesActualPrePushAndFailureRevokesIt(t *testing.T) {
	dir := fixture(t, "true")
	write(t, filepath.Join(dir, "go.mod"), "module gatefixture\n\ngo 1.21\n")
	write(t, filepath.Join(dir, "main.go"), "package main\nfunc main() {}\n")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-qm", "Go fixture")
	if err := Run(dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	hook, err := filepath.Abs("../../scripts/hooks/pre-push")
	if err != nil {
		t.Fatal(err)
	}
	push := func() error {
		cmd := exec.Command("sh", hook)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader("refs/heads/master " + fixtureGit(t, dir, "rev-parse", "HEAD") + " refs/heads/master 0000000000000000000000000000000000000000\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("hook: %s", out)
		}
		return err
	}
	if err := push(); err != nil {
		t.Fatalf("real attestation rejected: %v", err)
	}
	write(t, filepath.Join(dir, "untracked.go"), "package main\n")
	if err := Run(dir, io.Discard); err == nil {
		t.Fatal("untracked source passed")
	}
	if err := push(); err == nil {
		t.Fatal("hook accepted a revoked pass")
	}
}
