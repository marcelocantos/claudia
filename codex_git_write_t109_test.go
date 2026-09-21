// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// 🎯T109: a Codex workspace-write sandbox keeps `<workdir>/.git` read-only
// unless the git directory is granted as a writable root of its own.
// These tests pin the grant (a `-c` override on the app-server's argv) and
// the refusal (a sandbox echoed without the root).
//
// 🎯T112: the grant is the spawner's to ask for, with Config.SandboxGitWrite.
// A writable .git/hooks runs outside the sandbox on the operator's next git
// command, so the default arm keeps .git protected — and says so.

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

// The grant is one `-c` override on the app-server's argv. It replaces
// the config.toml list, so the caller's own roots must ride along.
func TestT109GitRootsRideTheAppServerArgv(t *testing.T) {
	got := codexSandboxArgs(codexSandboxTuning{
		WritableRoots: []string{"/Users/x/.jevons/gates", "/repo/.git"},
		GitRoots:      []string{"/repo/.git", "/main/.git"},
	})
	want := []string{"-c", `sandbox_workspace_write.writable_roots=["/Users/x/.jevons/gates", "/repo/.git", "/main/.git"]`}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	// No git roots, no override: the caller's roots stay in config.toml
	// alone, as T598 left them.
	if got := codexSandboxArgs(codexSandboxTuning{WritableRoots: []string{"/tmp/gates"}, NetworkAccess: true}); got != nil {
		t.Fatalf("args without git roots = %q", got)
	}
	// And config.toml is not where the git roots go.
	if toml := codexSandboxTOML(codexSandboxTuning{GitRoots: []string{"/repo/.git"}}); toml != "" {
		t.Fatalf("git roots leaked into config.toml:\n%s", toml)
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

// t109LastArgv is the argv the fake app-server was started with.
func t109LastArgv(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("app-server argv: %v", err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatalf("app-server argv %q: %v", raw, err)
	}
	return argv
}

// The whole path: Start in a repo with workspace-write puts the grant on
// the app-server's argv, and the (fake) app-server echoes it back.
func TestT109HermeticStartGrantsTheRepoGitDir(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("FAKE_CODEX_LAST_ARGV", argvLog)
	repo := t109Repo(t)

	agent, err := Start(Config{
		Provider:        ProviderCodex,
		WorkDir:         repo,
		SandboxMode:     "workspace-write",
		SandboxGitWrite: true,
		TermLogPath:     "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	want := []string{"app-server", "-c", "sandbox_workspace_write.writable_roots=[" + strconv.Quote(filepath.Join(repo, ".git")) + "]"}
	if got := t109LastArgv(t, argvLog); !slices.Equal(got, want) {
		t.Fatalf("app-server argv = %q, want %q", got, want)
	}
	// A bare seat keeps its threads in the user's CODEX_HOME. The grant
	// must not move it into a private one, or its next resume finds none.
	if dirExists(exclusiveCodexHomeDir(agent.SessionID())) {
		t.Fatal("the git grant gave a bare seat a private CODEX_HOME")
	}
}

// A read-only seat is not given write access to anything, .git included.
func TestT109ReadOnlySeatGetsNoGitGrant(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("FAKE_CODEX_LAST_ARGV", argvLog)

	agent, err := Start(Config{Provider: ProviderCodex, WorkDir: t109Repo(t), TermLogPath: "-"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	if got := t109LastArgv(t, argvLog); !slices.Equal(got, []string{"app-server"}) {
		t.Fatalf("read-only app-server argv = %q, want a bare app-server", got)
	}
}

// The caller's own roots live in config.toml, and `-c` replaces that list.
// A seat with both must end up with both.
func TestT109HermeticGrantKeepsTheCallersRoots(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("FAKE_CODEX_LAST_ARGV", argvLog)
	repo := t109Repo(t)
	gates := filepath.Join(t.TempDir(), "gates")

	agent, err := Start(Config{
		Provider:             ProviderCodex,
		WorkDir:              repo,
		SandboxMode:          "workspace-write",
		SandboxGitWrite:      true,
		SandboxWritableRoots: []string{gates},
		TermLogPath:          "-",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()
	want := "sandbox_workspace_write.writable_roots=[" + strconv.Quote(gates) + ", " + strconv.Quote(filepath.Join(repo, ".git")) + "]"
	if got := t109LastArgv(t, argvLog); !slices.Contains(got, want) {
		t.Fatalf("app-server argv = %q, want it to carry %s", got, want)
	}
}

// A CLI that ignores the override leaves .git read-only. Start must say so
// rather than hand back a seat that cannot commit.
func TestT109HermeticStartRefusesWhenTheGrantIsIgnored(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("FAKE_CODEX_DROP_WRITABLE_ROOTS", "1")
	repo := t109Repo(t)

	agent, err := Start(Config{
		Provider:        ProviderCodex,
		WorkDir:         repo,
		SandboxMode:     "workspace-write",
		SandboxGitWrite: true,
		TermLogPath:     "-",
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

// t112CaptureLogs routes slog into a buffer for the rest of the test.
func t112CaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &logs
}

// The default arm: no SandboxGitWrite, no grant — and no silence either.
// The seat starts, .git keeps Codex's protection, and the log names the
// directory and the field that would lift it.
func TestT112DefaultKeepsGitProtectedAndSaysSo(t *testing.T) {
	bin := writeFakeCodexAppServer(t)
	t.Setenv("CODEX_BIN", bin)
	writeFakeCodexSubscriptionAuth(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	argvLog := filepath.Join(t.TempDir(), "argv.json")
	t.Setenv("FAKE_CODEX_LAST_ARGV", argvLog)
	logs := t112CaptureLogs(t)
	repo := t109Repo(t)

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

	if got := t109LastArgv(t, argvLog); !slices.Equal(got, []string{"app-server"}) {
		t.Fatalf("app-server argv = %q: .git was granted to a seat that did not ask for it", got)
	}
	for _, want := range []string{".git stays read-only", filepath.Join(repo, ".git"), "SandboxGitWrite"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log does not mention %q — a read-only .git must not be silent:\n%s", want, logs)
		}
	}
}

// Outside a repository there is no .git to protect and nothing to say.
func TestT112DefaultIsQuietOutsideARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(canonicalPath(dir)))
	logs := t112CaptureLogs(t)
	tuning, err := codexSandboxTuningFor(agentStartRequest{
		WorkDir: dir,
		Config:  Config{SandboxMode: "workspace-write"},
	})
	if err != nil || len(tuning.GitRoots) != 0 {
		t.Fatalf("tuning = %+v, err = %v", tuning, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged about a .git that does not exist:\n%s", logs)
	}
}

// The decision itself, both arms, without a process.
func TestT112GrantFollowsTheField(t *testing.T) {
	repo := t109Repo(t)
	t112CaptureLogs(t)
	for _, tc := range []struct {
		name  string
		cfg   Config
		roots []string
	}{
		{"unset", Config{SandboxMode: "workspace-write"}, nil},
		{"set", Config{SandboxMode: "workspace-write", SandboxGitWrite: true}, []string{filepath.Join(repo, ".git")}},
		// danger-full-access carves nothing out; there is nothing to grant.
		{"full-access", Config{SandboxMode: "danger-full-access", SandboxGitWrite: true}, nil},
	} {
		tuning, err := codexSandboxTuningFor(agentStartRequest{WorkDir: repo, Config: tc.cfg})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !slices.Equal(tuning.GitRoots, tc.roots) {
			t.Fatalf("%s: git roots = %q, want %q", tc.name, tuning.GitRoots, tc.roots)
		}
	}
}

// Asking for a writable .git on a seat that writes nothing is a
// contradiction, and dropping the field would be the silence again.
func TestT112GitWriteOnAReadOnlySeatIsRefused(t *testing.T) {
	for _, mode := range []string{"", "read-only"} {
		err := codexSessionPrecheck(agentStartRequest{
			Config: Config{SandboxMode: mode, SandboxGitWrite: true},
		})
		if err == nil {
			t.Fatalf("SandboxMode %q with SandboxGitWrite was accepted", mode)
		}
		for _, want := range []string{"SandboxGitWrite", "workspace-write"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal does not name %q: %v", want, err)
			}
		}
	}
	if err := codexSessionPrecheck(agentStartRequest{
		Config: Config{SandboxMode: "workspace-write", SandboxGitWrite: true},
	}); err != nil {
		t.Fatalf("workspace-write with SandboxGitWrite refused: %v", err)
	}
}

// A seat that asked for the grant in a workdir git cannot read is refused
// by name rather than started without it.
func TestT112UnresolvableGitDirRefusesOnlyWhenAsked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required")
	}
	t112CaptureLogs(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	_, err := codexSandboxTuningFor(agentStartRequest{
		WorkDir: missing,
		Config:  Config{SandboxMode: "workspace-write", SandboxGitWrite: true},
	})
	if err == nil || !strings.Contains(err.Error(), "SandboxGitWrite") {
		t.Fatalf("err = %v, want a refusal naming SandboxGitWrite", err)
	}
	if _, err := codexSandboxTuningFor(agentStartRequest{
		WorkDir: missing,
		Config:  Config{SandboxMode: "workspace-write"},
	}); err != nil {
		t.Fatalf("a seat that did not ask was refused: %v", err)
	}
}

// The field survives the registry and the broker wire; a relaunch that
// dropped it would bring the seat back unable to commit.
func TestT112FieldSurvivesTheAgentDef(t *testing.T) {
	def := AgentDef{Name: "t112", Provider: ProviderCodex, SandboxMode: "workspace-write", SandboxGitWrite: true}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sandbox_git_write":true`) {
		t.Fatalf("AgentDef JSON lacks sandbox_git_write: %s", raw)
	}
	var back AgentDef
	if err := json.Unmarshal(raw, &back); err != nil || !back.SandboxGitWrite {
		t.Fatalf("round trip lost the field: %+v, %v", back, err)
	}
	// The hop a relaunch takes: registry definition to Start config.
	if cfg := registryConfig(&back, false); !cfg.SandboxGitWrite {
		t.Fatalf("registryConfig dropped SandboxGitWrite: %+v", cfg)
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
		Provider:        ProviderCodex,
		WorkDir:         repo,
		SandboxMode:     "workspace-write",
		SandboxGitWrite: true,
		TermLogPath:     "-",
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
