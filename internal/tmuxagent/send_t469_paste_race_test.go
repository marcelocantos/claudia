// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestT469SharedBufferNameRacesUnderBarrier is the RED oracle for the
// pre-fix tree: every send used the fixed literal buffer name
// "claudia-send". With a barrier between load and paste, all N loads
// overwrite the same buffer, then the first paste-buffer -d deletes it
// and the remaining N-1 fail with "no buffer".
//
// This is the 2026-08-15 failure mode (jevons 🎯T469 / claudia 🎯T46):
// concurrent jevons_agent_start/send colliding on one name.
func TestT469SharedBufferNameRacesUnderBarrier(t *testing.T) {
	_ = testServerSocket(t)
	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	const n = 12
	windows := spawnPasteSinks(t, n)
	payloads := make([]string, n)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("T469-shared-payload-%02d-UNIQUE", i)
	}

	var loadBarrier, pasteBarrier sync.WaitGroup
	loadBarrier.Add(n)
	pasteBarrier.Add(n)

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = pasteViaNamedBuffer(windows[i], payloads[i], "claudia-send", func() {
				loadBarrier.Done()
				loadBarrier.Wait() // every sibling has loaded
				pasteBarrier.Done()
				pasteBarrier.Wait() // release pastes together
			})
		}(i)
	}
	wg.Wait()

	missing := 0
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		if isNoBufferErr(err) {
			missing++
			continue
		}
		t.Errorf("unexpected paste error: %v", err)
	}
	if missing == 0 {
		t.Fatalf("expected shared-name race to yield 'no buffer' under barrier; got %d successes out of %d (oracle must stay RED on pre-fix path)", ok, n)
	}
	if ok != 1 {
		t.Logf("shared-name barrier: %d success, %d no-buffer (typical: 1 success after -d)", ok, missing)
	}
}

// TestT469ConcurrentUniqueNamesUnderBarrier is the GREEN mechanism
// oracle: unique per-send buffer names survive the same all-loads-
// before-any-paste barrier that makes the shared name fail, and each
// pane sink receives only its own payload.
func TestT469ConcurrentUniqueNamesUnderBarrier(t *testing.T) {
	_ = testServerSocket(t)
	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	const n = 16
	outdir := t.TempDir()
	windows := spawnPasteFileSinks(t, outdir, n)

	payloads := make([]string, n)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("T469-unique-payload-%02d-MARKER\n", i)
	}

	var loadBarrier, pasteBarrier sync.WaitGroup
	loadBarrier.Add(n)
	pasteBarrier.Add(n)

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = pasteViaNamedBuffer(windows[i], payloads[i], uniquePasteBufferName(), func() {
				loadBarrier.Done()
				loadBarrier.Wait()
				pasteBarrier.Done()
				pasteBarrier.Wait()
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("send %d: %v", i, err)
		}
		if isNoBufferErr(err) {
			t.Errorf("send %d hit buffer-missing — shared-name race regresses", i)
		}
	}
	if t.Failed() {
		return
	}

	for i, want := range payloads {
		marker := strings.TrimSuffix(want, "\n")
		path := filepath.Join(outdir, fmt.Sprintf("sink-%02d.txt", i))
		got, err := waitFileContains(path, marker, 5*time.Second)
		if err != nil {
			t.Errorf("sink %d: %v (got=%q)", i, err, got)
			continue
		}
		for j, other := range payloads {
			if j == i {
				continue
			}
			sib := strings.TrimSuffix(other, "\n")
			if strings.Contains(got, sib) {
				t.Errorf("sink %d contains sibling %d payload %q", i, j, sib)
			}
		}
	}
}

// TestT469PasteViaBufferUsesUniqueNames guards the production entry
// point: concurrent pasteViaBuffer (no test hook) must also be clean.
func TestT469PasteViaBufferUsesUniqueNames(t *testing.T) {
	_ = testServerSocket(t)
	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	const n = 16
	windows := spawnPasteSinks(t, n)
	payloads := make([]string, n)
	for i := range payloads {
		payloads[i] = fmt.Sprintf("prod-path-payload-%02d\nline-two", i)
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = pasteViaBuffer(windows[i], payloads[i])
		}(i)
	}
	wg.Wait()

	missing := 0
	for i, err := range errs {
		if err == nil {
			continue
		}
		t.Errorf("pasteViaBuffer %d: %v", i, err)
		if isNoBufferErr(err) {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("%d/%d pasteViaBuffer calls hit 'no buffer' — unique buffer name regresses", missing, n)
	}
}

// TestT469PasteFailureDeletesBuffer ensures a failed paste does not
// leave a named buffer behind (clean rollback residual).
func TestT469PasteFailureDeletesBuffer(t *testing.T) {
	sock := testServerSocket(t)
	if err := EnsureServer(); err != nil {
		t.Fatalf("EnsureServer: %v", err)
	}

	buf := uniquePasteBufferName()
	err := pasteViaNamedBuffer("@99999-missing", "cleanup-probe", buf, nil)
	if err == nil {
		t.Fatal("expected paste to missing window to fail")
	}
	if isNoBufferErr(err) {
		t.Fatalf("unexpected no-buffer on missing window: %v", err)
	}

	out, listErr := exec.Command("tmux", "-S", sock, "list-buffers", "-F", "#{buffer_name}").CombinedOutput()
	listed := string(out)
	if listErr != nil && strings.Contains(listed, buf) {
		t.Fatalf("failed paste left buffer %q listed: %s", buf, listed)
	}
	for _, line := range strings.Split(strings.TrimSpace(listed), "\n") {
		if line == buf {
			t.Fatalf("failed paste left orphan buffer %q", buf)
		}
	}
}

func isNoBufferErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no buffer") || strings.Contains(s, "buffer not found")
}

func spawnPasteSinks(t *testing.T, n int) []string {
	t.Helper()
	workdir, err := os.MkdirTemp("", "t469sink")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workdir) })

	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id, err := SpawnWindow(workdir, fmt.Sprintf("t469-%02d", i), "cat", nil)
		if err != nil {
			t.Fatalf("SpawnWindow %d: %v", i, err)
		}
		ids[i] = id
		wid := id
		t.Cleanup(func() { _ = KillWindow(wid) })
	}
	return ids
}

func spawnPasteFileSinks(t *testing.T, outdir string, n int) []string {
	t.Helper()
	workdir, err := os.MkdirTemp("", "t469filesink")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workdir) })

	ids := make([]string, n)
	for i := 0; i < n; i++ {
		path := filepath.Join(outdir, fmt.Sprintf("sink-%02d.txt", i))
		id, err := SpawnWindow(workdir, fmt.Sprintf("t469f-%02d", i), "sh",
			[]string{"-c", "cat >> " + shellSingleQuote(path)})
		if err != nil {
			t.Fatalf("SpawnWindow file sink %d: %v", i, err)
		}
		ids[i] = id
		wid := id
		t.Cleanup(func() { _ = KillWindow(wid) })
	}
	return ids
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func waitFileContains(path, substr string, budget time.Duration) (string, error) {
	deadline := time.Now().Add(budget)
	var last string
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			last = string(b)
			if strings.Contains(last, substr) {
				return last, nil
			}
		}
		if time.Now().After(deadline) {
			if last == "" {
				return "", fmt.Errorf("timeout waiting for %q in %s (file missing or empty)", substr, path)
			}
			return last, fmt.Errorf("timeout waiting for %q in %s", substr, path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
