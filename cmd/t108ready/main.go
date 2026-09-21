// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// t108ready measures how long a cold Claude Code seat takes to draw a
// ready composer, and what the pane shows on the way, at whatever load
// the host is carrying (🎯T108). It spawns claude in a window on the
// claudia tmux server with the argv shape a Session uses, polls
// capture-pane at the production interval, and prints one JSON line per
// sample. The window is killed as soon as the composer is live, so a
// sample submits no prompt and spends no model allowance.
//
// The poll runs well past the 30s production bound on purpose: the
// subject is the true latency, including samples the bound would have
// refused. Each line records the phases the readiness verdict is built
// from — first ink, first composer (splash or live), live composer,
// last frame change — and the 1-minute load average at spawn and ready.
//
//	go run ./cmd/t108ready -n 5 -gap 20s >> samples.jsonl
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// productionPoll is readyPollInterval in agent.go.
const productionPoll = 50 * time.Millisecond

type sample struct {
	At         string  `json:"at"`
	LoadSpawn  float64 `json:"load1_spawn"`
	LoadReady  float64 `json:"load1_ready"`
	SpawnMS    int64   `json:"spawn_ms"`
	FirstInkMS int64   `json:"first_ink_ms"`      // first non-blank frame
	ComposerMS int64   `json:"first_composer_ms"` // splash or live box drawn
	ReadyMS    int64   `json:"ready_ms"`          // MatchReady
	LastEditMS int64   `json:"last_change_ms"`    // last frame change before ready (or deadline)
	MaxQuietMS int64   `json:"max_quiet_ms"`      // longest run of identical frames after first ink
	Frames     int     `json:"frames"`
	Reason     string  `json:"reason,omitempty"` // NotReadyReason of last frame when never ready
	Err        string  `json:"err,omitempty"`
}

func main() {
	n := flag.Int("n", 1, "samples to take")
	gap := flag.Duration("gap", 15*time.Second, "pause between samples")
	limit := flag.Duration("limit", 180*time.Second, "give up on a sample after this long")
	workdir := flag.String("workdir", ".", "working directory for the spawned claude")
	flag.Parse()

	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		die("claude not on PATH: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	for i := 0; i < *n; i++ {
		if i > 0 {
			time.Sleep(*gap)
		}
		s := measure(claudeBin, *workdir, *limit)
		if err := enc.Encode(s); err != nil {
			die("encode: %v", err)
		}
	}
}

func measure(claudeBin, workdir string, limit time.Duration) sample {
	s := sample{At: time.Now().Format(time.RFC3339), LoadSpawn: load1(),
		FirstInkMS: -1, ComposerMS: -1, ReadyMS: -1, LastEditMS: -1}
	sessionID := newUUID()
	args := []string{
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", "Agent,SendMessage,EnterWorktree",
		"--session-id", sessionID,
	}
	start := time.Now()
	windowID, err := tmuxagent.SpawnWindow(workdir, tmuxagent.SessionWindowName(sessionID), claudeBin, args)
	if err != nil {
		s.Err = err.Error()
		return s
	}
	defer tmuxagent.KillWindow(windowID)
	s.SpawnMS = time.Since(start).Milliseconds()

	ms := func() int64 { return time.Since(start).Milliseconds() }
	var last []byte
	deadline := start.Add(limit)
	for time.Now().Before(deadline) {
		frame, err := tmuxagent.CapturePane(windowID)
		if err != nil {
			s.Err = err.Error()
			time.Sleep(productionPoll)
			continue
		}
		s.Err = ""
		s.Frames++
		if !bytes.Equal(frame, last) {
			now := ms()
			if s.FirstInkMS >= 0 && now-s.LastEditMS > s.MaxQuietMS {
				s.MaxQuietMS = now - s.LastEditMS
			}
			s.LastEditMS = now
			last = frame
		}
		if s.FirstInkMS < 0 && len(bytes.TrimSpace(frame)) > 0 {
			s.FirstInkMS = ms()
		}
		if s.ComposerMS < 0 && (tmuxagent.MatchStartupSplash(frame) || tmuxagent.MatchReady(frame)) {
			s.ComposerMS = ms()
		}
		if tmuxagent.MatchReady(frame) {
			s.ReadyMS = ms()
			s.LoadReady = load1()
			return s
		}
		if tmuxagent.MatchStartupMenu(frame) {
			tmuxagent.SendKeys(windowID, "")
			time.Sleep(400 * time.Millisecond)
			continue
		}
		time.Sleep(productionPoll)
	}
	s.LoadReady = load1()
	s.Reason = tmuxagent.NotReadyReason(last)
	fmt.Fprintf(os.Stderr, "sample %s never ready; last frame:\n%s\n", sessionID, last)
	return s
}

// load1 is the 1-minute load average, from vm.loadavg ("{ 1m 5m 15m }").
func load1() float64 {
	out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return -1
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
	if len(f) == 0 {
		return -1
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return -1
	}
	return v
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		die("rand: %v", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
