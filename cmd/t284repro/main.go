// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command t284repro measures whether the readiness signal is
// trustworthy: does WaitReady returning mean the TUI will actually
// accept and run the next turn?
//
// It launches N Claude sessions, waits for ready, immediately sends a
// probe turn, and requires the agent to ANSWER it. Answering is the
// only sound oracle here — the obvious "did the composer clear?" check
// passes vacuously when the splash screen swallows the keystrokes,
// because swallowed text and submitted text both leave an empty
// composer behind.
//
// Run against a fresh workdir: the startup placeholder ("Try \"fix
// lint errors\"") only renders when Claude Code has no history for the
// directory, and that placeholder frame is exactly the one the old
// ready pattern matched.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// sentinelReply is what the probe turn asks the agent to say back. It
// is deliberately not a word the TUI chrome ever prints on its own.
const sentinelReply = "T284PONG"

func main() {
	trials := flag.Int("trials", 5, "number of launch trials")
	conc := flag.Int("conc", 1, "sessions launched concurrently per trial (load)")
	model := flag.String("model", "haiku", "model for the probe session")
	grace := flag.Duration("grace", 60*time.Second, "how long to wait for the probe answer")
	settle := flag.Duration("settle", 0, "artificial settle after ready (simulates the jevons workaround)")
	workdir := flag.String("workdir", "", "workdir for probe sessions (default: a fresh temp dir per session)")
	dump := flag.String("dump", "", "directory to write first-ready and failure frames into")
	flag.Parse()

	if err := tmuxagent.EnsureServer(); err != nil {
		die("ensure tmux server: %v", err)
	}
	if *dump != "" {
		if err := os.MkdirAll(*dump, 0o755); err != nil {
			die("mkdir dump: %v", err)
		}
	}

	falseReady, total := 0, 0

	for i := 1; i <= *trials; i++ {
		var wg sync.WaitGroup
		results := make([]string, *conc)
		bad := make([]bool, *conc)

		for j := range results {
			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				name := fmt.Sprintf("t284-%d-%d", i, j)
				ok, msg := runOne(name, *workdir, *model, *grace, *settle, *dump)
				results[j], bad[j] = msg, !ok
			}(j)
		}
		wg.Wait()

		for j, msg := range results {
			total++
			if bad[j] {
				falseReady++
			}
			fmt.Printf("trial %d.%d: %s\n", i, j, msg)
		}
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Printf("\nRESULT: %d/%d false ready\n", falseReady, total)
	if falseReady > 0 {
		os.Exit(1)
	}
}

func runOne(name, workdir, model string, grace, settle time.Duration, dump string) (bool, string) {
	if workdir == "" {
		d, err := os.MkdirTemp("", "t284-")
		if err != nil {
			return false, fmt.Sprintf("mkdtemp: %v", err)
		}
		defer os.RemoveAll(d)
		workdir = d
	}

	windowID, err := tmuxagent.SpawnWindow(workdir, name, "claude",
		[]string{"--permission-mode", "bypassPermissions", "--model", model})
	if err != nil {
		return false, fmt.Sprintf("spawn failed: %v", err)
	}
	defer tmuxagent.KillWindow(windowID)

	readyAt, err := tmuxagent.WaitReady(windowID, 50*time.Millisecond, 60*time.Second)
	if err != nil {
		return false, fmt.Sprintf("WaitReady FAILED: %v", err)
	}

	// Record the frame that satisfied the ready signal — this is the
	// evidence for or against the discriminator.
	readyFrame, _ := tmuxagent.CapturePane(windowID)
	if dump != "" && readyFrame != nil {
		os.WriteFile(filepath.Join(dump, name+".ready.txt"), readyFrame, 0o644)
	}
	// Signal oracle: ready must never land on the startup splash. This
	// fails deterministically on the pre-fix signal, where the
	// placeholder frame is the very frame WaitReady returns on.
	if tmuxagent.MatchStartupSplash(readyFrame) {
		return false, fmt.Sprintf("FALSE READY (ready@%s on the startup splash — composer drawn but dead)\n%s",
			readyAt.Round(time.Millisecond), tail(string(readyFrame), 4))
	}

	time.Sleep(settle)

	if err := tmuxagent.SendKeys(windowID, "Reply with exactly this word and nothing else: "+sentinelReply); err != nil {
		return false, fmt.Sprintf("send failed: %v", err)
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		frame, err := tmuxagent.CapturePane(windowID)
		if err == nil && answered(string(frame)) {
			return true, fmt.Sprintf("ok (ready@%s, turn answered)", readyAt.Round(time.Millisecond))
		}
		time.Sleep(200 * time.Millisecond)
	}

	frame, _ := tmuxagent.CapturePane(windowID)
	if dump != "" {
		os.WriteFile(filepath.Join(dump, name+".stuck.txt"), frame, 0o644)
	}
	return false, fmt.Sprintf("FALSE READY (ready@%s, no answer within %s — keystrokes swallowed by the splash)\n--- ready frame tail ---\n%s\n--- final frame tail ---\n%s\n------------------",
		readyAt.Round(time.Millisecond), grace, tail(string(readyFrame), 5), tail(string(frame), 8))
}

// answered reports whether the agent produced the sentinel above the
// composer, i.e. as transcript output rather than as text still sitting
// in the input box. Anything at or below the last ❯ line is composer.
func answered(frame string) bool {
	lines := strings.Split(frame, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "❯") {
			return strings.Contains(strings.Join(lines[:i], "\n"), sentinelReply)
		}
	}
	return strings.Contains(frame, sentinelReply)
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n "), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}
