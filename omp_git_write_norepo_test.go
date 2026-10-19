// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// A code worker whose workdir is not a repository asks for SandboxGitWrite
// the way Jevons mints every Codex work seat. There is no git directory to
// grant and none to protect, so the sidecar seat launches workspace-write
// with an empty grant — what the direct Codex route has always done. It
// used to be refused ("sandbox git write: ... is not in a git repository"),
// which failed Jevons journey J21's codex leg and any such production spawn.
func TestOMPGitWriteOutsideARepositoryLaunches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	f := startOMPFixture(t)
	dir := canonicalPath(t.TempDir())
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	r, err := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(AgentDef{
		Name: "t510-codex", WorkDir: dir, SessionID: "sid-norepo", Provider: Provider(omp.OpenAICodex), Model: "gpt-6-sol",
		Purpose: PurposeWork, SandboxMode: "workspace-write", SandboxGitWrite: true,
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := r.Launch("t510-codex")
	if err != nil {
		t.Fatalf("launch outside a repository with SandboxGitWrite: %v", err)
	}
	defer agent.Stop()
	load := f.nextLoad(t)
	if load.Sandbox == nil || load.Sandbox.Mode != "workspace-write" ||
		!samePaths(load.Sandbox.WritableRoots, []string{bullseyeLedgerRoot()}) || len(load.Sandbox.Protected) != 0 {
		t.Fatalf("load sandbox = %+v, want workspace-write with only the ledger root and nothing protected", load.Sandbox)
	}
}

// What stays refused: a workdir git cannot answer for at all. The grant was
// asked for and nobody can say what it would cover.
func TestOMPGitWriteUnresolvableWorkdirStillRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	_, err := ompSandboxFor(Config{Provider: ProviderCodex, WorkDir: missing, SandboxMode: "workspace-write", SandboxGitWrite: true})
	if err == nil || !strings.Contains(err.Error(), "sandbox git write") {
		t.Fatalf("err = %v, want a refusal naming sandbox git write", err)
	}
}
