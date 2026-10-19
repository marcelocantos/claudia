// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package testhome

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/testctlenv"
)

const (
	modulePath = "github.com/marcelocantos/claudia"
	self       = modulePath + "/internal/testhome"
)

// TestEveryTestBinaryLinksTesthome is the ratchet. The set is every test
// binary the go command builds for this module, not the ones that look as if
// they reach the state directory: HOME is read by package os itself and by
// every child process a test starts, so reachability is not decidable from
// imports, and a hand-kept list is how the chains directory filled up.
func TestEveryTestBinaryLinksTesthome(t *testing.T) {
	cmd := exec.Command("go", "list", "-e", "-test", "-f",
		`{{if and (eq .Name "main") (not .ForTest)}}{{.ImportPath}}{{range .Deps}} {{.}}{{end}}{{end}}`, "./...")
	cmd.Dir = moduleRoot(t)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	binaries := 0
	var missing []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasSuffix(fields[0], ".test") {
			continue
		}
		binaries++
		linked := false
		for _, dep := range fields[1:] {
			linked = linked || dep == self
		}
		if !linked {
			missing = append(missing, strings.TrimSuffix(fields[0], ".test"))
		}
	}
	if binaries == 0 {
		t.Fatal("go list named no test binaries; the census is broken, not clean")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("%d of %d test packages run under the owner's HOME, so claudia's default state "+
			"directory (~/.local/state/claudia) is reachable from them; add a _test.go file with\n"+
			"\timport _ %q\nto:\n  %s",
			len(missing), binaries, self, strings.Join(missing, "\n  "))
	}
	t.Logf("%d test binaries, all on a private HOME", binaries)
}

// TestInitMovedHomeOffTheAccount checks the effect, not the import: after
// init, HOME and XDG_STATE_HOME name the private directory, neither is the
// account's real home or state directory as the user database records it,
// and the private home is outside the roots the sidecar seatbelt grants.
func TestInitMovedHomeOffTheAccount(t *testing.T) {
	if Dir == "" {
		t.Skip("live run: HOME left as the owner's")
	}
	if got := os.Getenv("HOME"); got != Dir {
		t.Fatalf("HOME = %q, want %q", got, Dir)
	}
	if got, want := os.Getenv("XDG_STATE_HOME"), filepath.Join(Dir, ".local", "state"); got != want {
		t.Fatalf("XDG_STATE_HOME = %q, want %q", got, want)
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		t.Skipf("no account home to compare against: %v", err)
	}
	if os.Getenv("HOME") == u.HomeDir {
		t.Errorf("HOME is still the account home %q", u.HomeDir)
	}
	realState := filepath.Join(u.HomeDir, ".local", "state")
	if rel, err := filepath.Rel(realState, os.Getenv("XDG_STATE_HOME")); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("XDG_STATE_HOME = %q is inside the account state dir %q", os.Getenv("XDG_STATE_HOME"), realState)
	}
	if os.Getenv(AccountHomeEnv) == u.HomeDir {
		for _, granted := range []string{"/tmp", "/private/tmp", "/var/folders", "/private/var/folders"} {
			if strings.HasPrefix(Dir, granted+"/") {
				t.Errorf("private HOME %q is under %s, which the sidecar seatbelt always grants", Dir, granted)
			}
		}
	}
	if os.Getenv(AccountHomeEnv) == "" {
		t.Errorf("%s not set", AccountHomeEnv)
	}
}

// TestLiveRunCoversEveryLiveGate pins the shape LiveRun matches to the
// registry it stands in for.
func TestLiveRunCoversEveryLiveGate(t *testing.T) {
	for _, gate := range testctlenv.LiveGates() {
		if !LiveRun([]string{gate + "=1"}) {
			t.Errorf("%s=1 is a live gate LiveRun does not recognise", gate)
		}
	}
	for _, kv := range []string{"CLAUDIA_LIVE=", "CLAUDIA_NO_BROKER=1", "JEVONS_LIVE=1", "HOME=/x"} {
		if LiveRun([]string{kv}) {
			t.Errorf("LiveRun(%q) = true, want false", kv)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		t.Fatal("not inside the claudia module")
	}
	return filepath.Dir(mod)
}
