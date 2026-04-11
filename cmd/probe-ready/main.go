// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// probe-ready instruments a fresh Claude Code session's startup
// and reports which of the signals my reverted detectReady was
// watching for actually fire, and when. Specifically:
//
//   - t_first_pty: first byte read from the PTY
//   - t_jsonl:     first time os.Stat(jsonlPath) succeeds
//   - t_quiet:     first time the PTY has been silent for 500ms
//                  after at least one byte has been seen
//
// These are the same milestones detectReady used. Running this in
// the foreground (outside a nested Claude Code session) isolates
// whether the earlier probe failed because the JSONL never
// appeared, because the PTY never went quiet, or both.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/marcelocantos/claudia"
)

func main() {
	duration := flag.Duration("d", 20*time.Second, "observation cap")
	quiescence := flag.Duration("q", 500*time.Millisecond, "quiescence window")
	flag.Parse()

	agent, err := claudia.Start(claudia.Config{WorkDir: "."})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start: %v\n", err)
		os.Exit(1)
	}
	defer agent.Stop()

	fmt.Printf("session:   %s\n", agent.SessionID())
	fmt.Printf("jsonl:     %s\n", agent.JSONLPath())
	fmt.Printf("term log:  %s\n\n", agent.TermLogPath())

	start := time.Now()

	// Track last PTY write time via an atomic so the main goroutine
	// can read it without locks.
	var lastPTYWrite atomic.Int64 // unix nanos
	var firstPTYWrite atomic.Int64
	var bytesSeen atomic.Int64

	_, ch := agent.SubscribeTerminal()
	defer agent.UnsubscribeTerminal(ch)

	go func() {
		for data := range ch {
			now := time.Now().UnixNano()
			if firstPTYWrite.Load() == 0 {
				firstPTYWrite.Store(now)
			}
			lastPTYWrite.Store(now)
			bytesSeen.Add(int64(len(data)))
		}
	}()

	var tJSONL, tQuiet time.Duration
	deadline := start.Add(*duration)

	for time.Now().Before(deadline) {
		// JSONL milestone.
		if tJSONL == 0 {
			if _, err := os.Stat(agent.JSONLPath()); err == nil {
				tJSONL = time.Since(start)
				fmt.Printf("%8s  JSONL file appeared\n", tJSONL.Round(time.Millisecond))
			}
		}
		// Quiescence milestone.
		if tQuiet == 0 {
			last := lastPTYWrite.Load()
			if last > 0 {
				sinceLast := time.Since(time.Unix(0, last))
				if sinceLast >= *quiescence {
					tQuiet = time.Since(start)
					fmt.Printf("%8s  PTY silent for %s (quiescence reached)\n",
						tQuiet.Round(time.Millisecond),
						(*quiescence).Round(time.Millisecond))
				}
			}
		}
		if tJSONL > 0 && tQuiet > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	fmt.Println()
	first := firstPTYWrite.Load()
	last := lastPTYWrite.Load()
	fmt.Printf("first PTY write: ")
	if first > 0 {
		fmt.Printf("%s\n", time.Duration(first-start.UnixNano()).Round(time.Millisecond))
	} else {
		fmt.Println("NEVER")
	}
	fmt.Printf("last PTY write:  ")
	if last > 0 {
		fmt.Printf("%s\n", time.Duration(last-start.UnixNano()).Round(time.Millisecond))
	} else {
		fmt.Println("NEVER")
	}
	fmt.Printf("bytes observed:  %d\n\n", bytesSeen.Load())

	fmt.Printf("t_jsonl: ")
	if tJSONL > 0 {
		fmt.Println(tJSONL.Round(time.Millisecond))
	} else {
		fmt.Println("NEVER (within cap)")
	}
	fmt.Printf("t_quiet: ")
	if tQuiet > 0 {
		fmt.Println(tQuiet.Round(time.Millisecond))
	} else {
		fmt.Println("NEVER (within cap)")
	}

	if tJSONL > 0 && tQuiet > 0 {
		fmt.Printf("\nboth milestones reached\n")
	} else {
		fmt.Printf("\nat least one milestone missed\n")
	}

	// Measure WaitReady end-to-end against a fresh agent, so the
	// latency reflects real detectReady behaviour rather than a
	// channel that was already closed while we were observing.
	fmt.Println("\n--- fresh agent, WaitReady latency ---")
	fresh, err := claudia.Start(claudia.Config{WorkDir: "."})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fresh start: %v\n", err)
		os.Exit(1)
	}
	defer fresh.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	waitStart := time.Now()
	err = fresh.WaitReady(ctx)
	fmt.Printf("WaitReady returned after %s: err=%v\n",
		time.Since(waitStart).Round(time.Millisecond), err)
}
