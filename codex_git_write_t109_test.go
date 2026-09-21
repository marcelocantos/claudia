// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 🎯T109: a Codex workspace-write sandbox keeps `<workdir>/.git` read-only
// unless the git directory is granted as a writable root of its own.
// These tests pin the grant (the config.toml stanza, the only channel that
// carries it) and the refusal (a sandbox echoed without the root).

// t109Repo makes a throwaway repository with one commit and returns its
// symlink-free path — t.TempDir() is under /var → /private/var on macOS.
func t109Repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	dir := canonicalPath(t.TempDir())
	t109Git(t, dir, "init", "-q", ".")
	t109Git(t, dir, "-c", "user.name=t109", "-c", "user.email=t109@example.invalid",
		"commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func t109Git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestT109GitRootsOfAMainCheckout(t *testing.T) {
	repo := t109Repo(t)
	got, err := codexGitWritableRoots(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(repo, ".git"); len(got) != 1 || got[0] != want {
		t.Fatalf("roots = %q, want [%q]", got, want)
	}
	// A subdirectory of the repo is still in the repo.
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := codexGitWritableRoots(sub); err != nil || len(got) != 1 || got[0] != filepath.Join(repo, ".git") {
		t.Fatalf("subdir roots = %q, %v", got, err)
	}
}

// A linked worktree's `.git` is a file. The directory that takes the
// writes is the main checkout's, outside the workdir — the shape that
// produced "could not create directory of '.git/worktrees/tree'".
func TestT109GitRootsOfALinkedWorktreeAreTheSharedGitDir(t *testing.T) {
	repo := t109Repo(t)
	linked := filepath.Join(canonicalPath(t.TempDir()), "linked")
	t109Git(t, repo, "worktree", "add", "-q", linked)

	got, err := codexGitWritableRoots(linked)
	if err != nil {
		t.Fatal(err)
	}
	// One root: <main>/.git already contains worktrees/linked.
	if want := filepath.Join(repo, ".git"); len(got) != 1 || got[0] != want {
		t.Fatalf("roots = %q, want [%q]", got, want)
	}
}

func TestT109NoGitRootsOutsideARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	dir := t.TempDir()
	// Stop discovery at the temp dir, wherever the host keeps it.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(canonicalPath(dir)))
	got, err := codexGitWritableRoots(dir)
	if err != nil || len(got) != 0 {
		t.Fatalf("roots = %q, err = %v; want none", got, err)
	}
}

func TestT109GitRootsAreWrittenAsWritableRoots(t *testing.T) {
	got := codexSandboxTOML(codexSandboxTuning{
		WritableRoots: []string{"/Users/x/.jevons/gates", "/repo/.git"},
		GitRoots:      []string{"/repo/.git", "/main/.git"},
	})
	want := `writable_roots = ["/Users/x/.jevons/gates", "/repo/.git", "/main/.git"]`
	if !strings.Contains(got, want) {
		t.Fatalf("stanza lacks %s:\n%s", want, got)
	}
	// Git roots alone still produce a stanza, and so still demand a home.
	if alone := codexSandboxTOML(codexSandboxTuning{GitRoots: []string{"/repo/.git"}}); !strings.Contains(alone, `writable_roots = ["/repo/.git"]`) {
		t.Fatalf("git roots alone wrote:\n%s", alone)
	}
}

// The response below is thread/start as codex-cli 0.155.0-alpha.9.2
// returned it on 2026-09-21, trimmed to the fields read. `sandbox` is a
// sibling of `thread`, not a member of it.
const t109LiveThreadStart = `{"id":1,"result":{"thread":{"id":"019f-t109","cwd":"/repo"},` +
	`"model":"gpt-6-astra","cwd":"/repo","approvalPolicy":"never",` +
	`"sandbox":{"type":"workspaceWrite","writableRoots":["/repo/.git"],"networkAccess":false,` +
	`"excludeTmpdirEnvVar":false,"excludeSlashTmp":false}}}`

func TestT109EffectiveSandboxIsReadFromTheLiveShape(t *testing.T) {
	got := parseEffectiveSandbox([]byte(t109LiveThreadStart))
	if got.Type != "workspaceWrite" || len(got.WritableRoots) != 1 || got.WritableRoots[0] != "/repo/.git" {
		t.Fatalf("live thread/start parsed as %+v", got)
	}
}

func TestT109MissingGitRootIsRefusedByName(t *testing.T) {
	granted := parseEffectiveSandbox([]byte(t109LiveThreadStart))
	if err := checkGitRootsGranted([]string{"/repo/.git"}, granted); err != nil {
		t.Fatalf("granted root refused: %v", err)
	}
	err := checkGitRootsGranted([]string{"/repo/.git", "/main/.git"}, granted)
	if err == nil {
		t.Fatal("a git root missing from the echoed sandbox was accepted")
	}
	for _, want := range []string{"/main/.git", "read-only", "workspace-write"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
	// read-only and full-access sandboxes do not carve .git out; nothing
	// reported is T598's silence, not a refusal.
	for _, other := range []codexEffectiveSandbox{{}, {Type: "readOnly"}, {Type: "dangerFullAccess"}} {
		if err := checkGitRootsGranted([]string{"/repo/.git"}, other); err != nil {
			t.Fatalf("sandbox %+v refused: %v", other, err)
		}
	}
}

// The whole path: Start in a repo with workspace-write writes the grant
// into the seat's CODEX_HOME, and the (fake) app-server echoes it back.
func TestT109HermeticStartGrantsTheRepoGitDir(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t109Repo(t)

	// No MCP servers and no caller tuning: the grant alone must demand
	// the private home it is written into.
	agent, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     repo,
		SandboxMode: "workspace-write",
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	cfg, err := os.ReadFile(filepath.Join(exclusiveCodexHomeDir(agent.SessionID()), "config.toml"))
	if err != nil {
		t.Fatalf("seat config.toml: %v", err)
	}
	want := "writable_roots = [" + strconv.Quote(filepath.Join(repo, ".git")) + "]"
	if !strings.Contains(string(cfg), want) {
		t.Fatalf("seat config.toml lacks %s:\n%s", want, cfg)
	}
}

// A read-only seat is not given write access to anything, .git included.
func TestT109ReadOnlySeatGetsNoGitGrant(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lastHome := filepath.Join(t.TempDir(), "home")
	t.Setenv("FAKE_CODEX_LAST_HOME", lastHome)

	agent, err := Start(Config{Provider: ProviderCodex, WorkDir: t109Repo(t), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if dirExists(exclusiveCodexHomeDir(agent.SessionID())) {
		t.Fatal("a read-only seat was given a private CODEX_HOME for a grant it must not have")
	}
}

// A CLI that ignores the stanza leaves .git read-only. Start must say so
// rather than hand back a seat that cannot commit.
func TestT109HermeticStartRefusesWhenTheGrantIsIgnored(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("FAKE_CODEX_DROP_WRITABLE_ROOTS", "1")
	repo := t109Repo(t)

	agent, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     repo,
		SandboxMode: "workspace-write",
		TermLogPath: "-",
	})
	if err == nil {
		agent.Stop()
		t.Fatal("Start succeeded with .git left read-only")
	}
	for _, want := range []string{filepath.Join(repo, ".git"), "read-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not name %q: %v", want, err)
		}
	}
}

// Live: a workspace-write seat commits and adds a worktree in its own
// repo. The outcome is read from the repository, not from the reply — a
// model that says "done" over a read-only .git has written nothing.
func TestCodexGitWriteLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveCodexBin(); err != nil {
		t.Skipf("codex binary not found: %v", err)
	}
	repo := t109Repo(t)
	t109Git(t, repo, "config", "user.name", "t109")
	t109Git(t, repo, "config", "user.email", "t109@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "note.txt"), []byte("t109\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	agent, err := Start(Config{
		Provider:    ProviderCodex,
		WorkDir:     repo,
		SandboxMode: "workspace-write",
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	const subject = "t109-live-commit"
	const worktree = "wt-t109"
	prompt := "Run exactly this shell command in the current directory and then reply with its output, nothing else:\n" +
		"git add note.txt && git commit -q -m " + subject + " && git worktree add -q " + worktree + " && echo T109-OK"
	if err := liveCodexTurn(t, agent, prompt); err != nil {
		t.Fatal(err)
	}

	log, err := exec.Command("git", "-C", repo, "log", "--format=%s", "-1").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := strings.TrimSpace(string(log)); got != subject {
		t.Fatalf("HEAD subject = %q, want %q — the seat could not commit", got, subject)
	}
	if !dirExists(filepath.Join(repo, ".git", "worktrees", worktree)) {
		t.Fatalf(".git/worktrees/%s missing — the seat could not add a worktree", worktree)
	}
}
