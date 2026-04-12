// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// probe-ready-tmux is the M1/M2 spike for 🎯T1.1 (tmux-backed Agent).
// It spawns Claude Code inside a window on the dedicated claudia
// tmux server, optionally dials a control-mode client to capture the
// terminal byte stream, polls capture-pane for the empty-prompt-box
// regex, prints the attach incantation, and tears down cleanly.
//
// With -control: the control-mode client dials immediately after
// window spawn (before readiness) so the full startup byte stream
// is captured. This validates that M2's parser delivers the same
// terminal bytes a human would see in `tmux attach`.
package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

func main() {
	workdir := flag.String("workdir", ".", "working directory for the spawned claude")
	timeout := flag.Duration("timeout", 30*time.Second, "overall readiness timeout")
	poll := flag.Duration("poll", 50*time.Millisecond, "capture-pane poll interval")
	model := flag.String("model", "", "optional --model override (e.g. haiku, sonnet)")
	keep := flag.Bool("keep", false, "don't kill the window on exit (so you can tmux attach and inspect)")
	doControl := flag.Bool("control", false, "dial a control-mode client before readiness to capture the full byte stream")
	flag.Parse()

	if err := tmuxagent.EnsureServer(); err != nil {
		die("ensure tmux server: %v", err)
	}

	disallowed := "Agent,TeamCreate,TeamDelete,SendMessage,EnterWorktree"
	args := []string{
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", disallowed,
	}
	if *model != "" {
		args = append(args, "--model", *model)
	}

	fmt.Printf("socket:  %s\n", tmuxagent.SocketPath())

	spawnStart := time.Now()
	windowID, err := tmuxagent.SpawnWindow(*workdir, "claudia-probe", "claude", args)
	if err != nil {
		die("spawn window: %v", err)
	}
	if !*keep {
		defer func() {
			if err := tmuxagent.KillWindow(windowID); err != nil {
				fmt.Fprintf(os.Stderr, "kill window: %v\n", err)
			}
		}()
	}

	fmt.Printf("window:  %s\n", windowID)
	fmt.Printf("spawn:   %s\n", time.Since(spawnStart).Round(time.Millisecond))
	fmt.Printf("attach:  tmux -S %s attach -t %s\n", tmuxagent.SocketPath(), windowID)
	fmt.Println()

	// --- M2 control-mode byte stream (optional) ---
	var controlTotal atomic.Int64
	var controlChunks atomic.Int64
	var firstChunkOnce sync.Once
	var firstChunk []byte
	var ctrl *tmuxagent.Control

	if *doControl {
		fmt.Printf("dialling control-mode client for pane byte stream...\n")
		ctrl, err = tmuxagent.DialControl(windowID)
		if err != nil {
			die("dial control: %v", err)
		}
		defer ctrl.Close()
		fmt.Printf("pane:    %s\n\n", ctrl.PaneID())

		go func() {
			for data := range ctrl.Bytes() {
				controlTotal.Add(int64(len(data)))
				controlChunks.Add(1)
				firstChunkOnce.Do(func() {
					firstChunk = append([]byte(nil), data...)
				})
			}
		}()
	}

	// --- M1 readiness detection ---
	fmt.Printf("polling capture-pane every %s for ready pattern (timeout %s)...\n", *poll, *timeout)
	latency, err := tmuxagent.WaitReady(windowID, *poll, *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("ready:   %s (end-to-end from spawn)\n",
		time.Since(spawnStart).Round(time.Millisecond))
	fmt.Printf("regex:   matched after %s of polling\n",
		latency.Round(time.Millisecond))

	// Let the control-mode drain a bit more after ready.
	if *doControl {
		time.Sleep(500 * time.Millisecond)
		fmt.Println()
		fmt.Printf("--- control-mode byte stream stats ---\n")
		fmt.Printf("chunks:  %d\n", controlChunks.Load())
		fmt.Printf("bytes:   %d\n", controlTotal.Load())
		if firstChunk != nil {
			fmt.Printf("first chunk preview (%d bytes, printable-only):\n%s\n",
				len(firstChunk), printablePreview(firstChunk, 240))
		}
	}

	if *keep {
		fmt.Printf("\nwindow kept alive. kill it manually with:\n")
		fmt.Printf("  tmux -S %s kill-window -t %s\n", tmuxagent.SocketPath(), windowID)
	}
}

func printablePreview(b []byte, maxBytes int) string {
	if len(b) > maxBytes {
		b = b[:maxBytes]
	}
	out := make([]rune, 0, len(b))
	for _, r := range string(b) {
		if unicode.IsPrint(r) || r == '\n' || r == '\t' {
			out = append(out, r)
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
