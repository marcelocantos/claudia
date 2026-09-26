// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGrokChildEnvPrefersUserGrokBin(t *testing.T) {
	home := t.TempDir()
	grokBin := filepath.Join(home, ".grok", "bin")
	localBin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(grokBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(localBin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", strings.Join([]string{"/usr/bin", grokBin, "/bin"}, string(os.PathListSeparator)))
	before := os.Getenv("PATH")

	env := grokChildEnv([]string{"GROK_HOME=" + filepath.Join(home, "exclusive")})
	if os.Getenv("PATH") != before {
		t.Fatal("grokChildEnv changed the process PATH")
	}
	got := envValue(env, "PATH")
	sep := string(os.PathListSeparator)
	wantPrefix := grokBin + sep + localBin + sep
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("PATH %q, want prefix %q", got, wantPrefix)
	}
	if strings.Count(got, grokBin) != 1 {
		t.Fatalf("PATH %q still lists %s later", got, grokBin)
	}
	if strings.Contains(got, filepath.Join(home, ".cargo", "bin")) {
		t.Fatalf("PATH %q includes a directory that does not exist", got)
	}
	if envValue(env, "GROK_HOME") != filepath.Join(home, "exclusive") {
		t.Fatalf("GROK_HOME = %q", envValue(env, "GROK_HOME"))
	}
}

func TestExplainGrokSidecarHandshakeNamesTheHelper(t *testing.T) {
	raw := fmt.Errorf("omp: sidecar said %q, want ready", "error")
	wrapped := fmt.Errorf("acp initialize: %w", fmt.Errorf("acp initialize: %w", raw))
	got := explainGrokSidecarHandshake(wrapped, "")
	msg := got.Error()
	for _, want := range []string{
		"omp",
		"error",
		`sidecar said "error", want ready`,
		"brew services restart claudia",
		"~/.grok/bin",
		"omp-sidecar.log",
		"command not found: setsid",
		"util-linux",
		"write EPIPE",
		"Cursor",
		"shim",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnosis %q missing %q", msg, want)
		}
	}
	if !strings.Contains(msg, "acp initialize:") {
		t.Fatalf("diagnosis dropped the provider error: %s", msg)
	}

	unrelated := fmt.Errorf("acp initialize: acp initialize: boom")
	if explainGrokSidecarHandshake(unrelated, "plain stderr") != unrelated {
		t.Fatal("a non-handshake failure was rewritten")
	}

	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	logPath := filepath.Join(state, "claudia", "omp-sidecar.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	logBody := "(eval):7: command not found: setsid\n\n[Uncaught Exception] Error: write EPIPE\n    at failWrite (node:net:60:30)\n"
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}
	quoted := explainGrokSidecarHandshake(raw, "")
	if !strings.Contains(quoted.Error(), "omp-sidecar.log:") || !strings.Contains(quoted.Error(), "command not found: setsid") || !strings.Contains(quoted.Error(), "write EPIPE") {
		t.Fatalf("log excerpt missing: %s", quoted)
	}

	closed := fmt.Errorf("acp initialize: grok acp: connection closed waiting for initialize")
	fromStderr := explainGrokSidecarHandshake(closed, "omp: sidecar said \"error\", want ready\n")
	if !strings.Contains(fromStderr.Error(), `helper "omp"`) || !strings.Contains(fromStderr.Error(), "child stderr:") {
		t.Fatalf("stderr handshake was not diagnosed: %s", fromStderr)
	}
}

func TestStartGrokACPExplainsSidecarHandshakeOnStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake grok uses a POSIX shell")
	}
	bin := writeFailingGrok(t, "#!/bin/sh\necho 'omp: sidecar said \"error\", want ready' >&2\nexit 1\n")
	_, err := startGrokACP(bin, t.TempDir(), "", "", false, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected startup failure")
	}
	msg := err.Error()
	for _, want := range []string{`helper "omp"`, `sidecar said "error", want ready`, "brew services restart claudia", "child stderr:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestStartGrokACPExplainsSidecarHandshakeOnJSONRPC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake grok uses a POSIX shell")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required")
	}
	dir := t.TempDir()
	py := filepath.Join(dir, "fail.py")
	script := `import json, sys
line = sys.stdin.readline()
msg = json.loads(line) if line else {}
sys.stdout.write(json.dumps({
    "jsonrpc": "2.0",
    "id": msg.get("id"),
    "error": {"code": -32000, "message": 'omp: sidecar said "error", want ready'},
}) + "\n")
sys.stdout.flush()
`
	if err := os.WriteFile(py, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeFailingGrok(t, "#!/bin/sh\nexec python3 \""+py+"\"\n")
	_, err := startGrokACP(bin, t.TempDir(), "", "", false, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected startup failure")
	}
	msg := err.Error()
	for _, want := range []string{"acp initialize:", `helper "omp"`, `sidecar said "error", want ready`, "brew services restart claudia"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestProviderChildEnvSuppliesSetsidWhenTheHostHasNone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid shim is a POSIX shell")
	}
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("perl required for the setsid shim")
	}
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 required to observe the session id")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Join(home, "no-such-dir"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	env := providerChildEnv(nil)
	shimDir := filepath.Join(home, "state", "claudia", "bin")
	shim := filepath.Join(shimDir, "setsid")
	if _, err := os.Stat(shim); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(envValue(env, "PATH"), shimDir+string(os.PathListSeparator)) {
		t.Fatalf("PATH %q, want shim dir first", envValue(env, "PATH"))
	}
	cmd := exec.Command(shim, "/usr/bin/python3", "-c", "import os; print(os.getpid(), os.getsid(0))")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim: %v\n%s", err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != fields[1] {
		t.Fatalf("shim did not start a session leader, output %q", out)
	}
}

func TestProviderChildEnvKeepsHostSetsid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("setsid shim is a POSIX shell")
	}
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(bin, "setsid")
	if err := os.WriteFile(host, []byte("#!/bin/sh\necho host-setsid\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	env := providerChildEnv(nil)
	if got := lookupExecutable(envValue(env, "PATH"), "setsid"); got != host {
		t.Fatalf("setsid resolved to %q, want the host binary %s", got, host)
	}
}

func TestStartCursorACPExplainsSidecarHandshake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake cursor agent uses a POSIX shell")
	}
	bin := writeFailingGrok(t, "#!/bin/sh\necho 'omp: sidecar said \"error\", want ready' >&2\nexit 1\n")
	_, err := startCursorACP(t.Context(), bin, t.TempDir(), "", "", false, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected startup failure")
	}
	msg := err.Error()
	for _, want := range []string{`helper "omp"`, `sidecar said "error", want ready`, "setsid", "Cursor", "child stderr:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func writeFailingGrok(t *testing.T, script string) string {
	t.Helper()
	t.Setenv(EnvGrokConnect, "")
	bin := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}
