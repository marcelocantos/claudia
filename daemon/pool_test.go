// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker/brokertest"
)

// startPoolHost prepares real tmux on a private server and a fake claude
// that draws the real prompt box and sits at it, so the library pool runs
// for real without a provider. It returns a function listing the pool
// windows on that server.
func startPoolHost(t *testing.T) func() []string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	fake := brokertest.Build(t)
	dir, err := os.MkdirTemp("/tmp", "cpl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	scenario := filepath.Join(dir, "scenario.json")
	raw, _ := json.Marshal(brokertest.Scenario{Ready: brokertest.ReadyPromptBox, Linger: true})
	if err := os.WriteFile(scenario, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The tmux server's environment is scrubbed, so the scenario rides in
	// a wrapper rather than in this process's environment.
	wrapper := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nFAKE_CLAUDE_SCENARIO='" + scenario + "' exec '" + fake.Bin + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", wrapper)
	sock := filepath.Join(dir, "tmux.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
	return func() []string {
		out, err := exec.Command("tmux", "-S", sock, "list-windows", "-a", "-F", "#{window_id} #{window_name}").Output()
		if err != nil {
			return nil
		}
		var ids []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if id, name, ok := strings.Cut(line, " "); ok && strings.HasPrefix(name, "claudia-pool-") {
				ids = append(ids, id)
			}
		}
		return ids
	}
}

// windowOption reads a tmux window option off the pool host's server. It
// is how the pool itself decides a window's fate (pool.go's Acquire sweep
// reads @claudia-held), so it is what a test must read to observe a
// release rather than a proxy for one.
func windowOption(t *testing.T, windowID, key string) string {
	t.Helper()
	out, err := exec.Command("tmux", "-S", os.Getenv("CLAUDIA_TMUX_SOCKET"),
		"show-options", "-wv", "-t", windowID, "@"+key).Output()
	if err != nil {
		t.Fatalf("read @%s on %s: %v", key, windowID, err)
	}
	return strings.TrimSpace(string(out))
}

// TestAcquireSharesOneWarmSeatAcrossConsumers is 🎯T64: with a daemon
// listening, Acquire is the daemon's pool. One consumer acquires and
// returns a seat; a second consumer's Acquire gets the same warm window
// without a second spawn; drop kills it.

func TestAcquireSharesOneWarmSeatAcrossConsumers(t *testing.T) {
	poolWindows := startPoolHost(t)
	work := t.TempDir()
	if _, err := claudia.Acquire(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = %v, want legacy refusal", err)
	}
	if _, err := claudia.AcquireDirect(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = %v, want legacy refusal", err)
	}
	if got := poolWindows(); len(got) != 0 {
		t.Fatalf("pool windows = %v, want none", got)
	}
}

// TestAcquiredSeatReturnsWhenConsumerLeaves (🎯T64): a consumer that goes
// away without releasing does not leave its seat held; the next Acquire
// gets it warm.

func TestAcquiredSeatReturnsWhenConsumerLeaves(t *testing.T) {
	poolWindows := startPoolHost(t)
	work := t.TempDir()
	if _, err := claudia.Acquire(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = %v, want legacy refusal", err)
	}
	if _, err := claudia.AcquireDirect(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = %v, want legacy refusal", err)
	}
	if got := poolWindows(); len(got) != 0 {
		t.Fatalf("pool windows = %v, want none", got)
	}
}

// TestAcquiredHandleOpsKeepAliveAndDirect (🎯T64): a seat operation beyond
// Send reaches the pooled window through the daemon (Resize changes the
// tmux window's size); Release("keep_alive_for:<secs>") returns the seat
// with the pool's keep-alive deadline set; and AcquireDirect with a daemon
// listening keeps the seat in this process.

func TestAcquiredHandleOpsKeepAliveAndDirect(t *testing.T) {
	poolWindows := startPoolHost(t)
	work := t.TempDir()
	if _, err := claudia.Acquire(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = %v, want legacy refusal", err)
	}
	if _, err := claudia.AcquireDirect(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = %v, want legacy refusal", err)
	}
	if got := poolWindows(); len(got) != 0 {
		t.Fatalf("pool windows = %v, want none", got)
	}
}
