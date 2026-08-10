// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command t28repro reproduces 🎯T28: a payload delivered to a
// claude-provider session that lands in the composer and is never
// submitted, while Send returns nil.
//
// It drives claudia as jevons does — Start(cfg), Send(A) to open a long
// turn, then Send(B) while that turn is still running — with a
// background sampler capturing the pane every 250ms. The verdict rests
// on pane frames only: the transcript is not a delivery oracle
// (jevons 🎯T417).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

func main() {
	size := flag.Int("bytes", 2048, "mid-turn payload size in bytes")
	workdir := flag.String("workdir", ".", "workdir for the probe session")
	dump := flag.String("dump", "", "directory to write captured frames into")
	settle := flag.Duration("settle", 20*time.Second, "how long to watch the pane after the mid-turn Send")
	openerFlag := flag.String("opener", "Without using any tools, write a detailed 3000-word essay about the number seven, in many short paragraphs.", "turn-A prompt that must still be running when B lands")
	dwell := flag.Duration("dwell", 8*time.Second, "how long to let turn A run before sending B")
	flag.Parse()

	if *dump != "" {
		if err := os.MkdirAll(*dump, 0o755); err != nil {
			die("mkdir dump: %v", err)
		}
	}

	a, err := claudia.Start(claudia.Config{Provider: claudia.ProviderClaude, WorkDir: *workdir})
	if err != nil {
		die("start: %v", err)
	}
	defer a.Stop()

	windowID := windowIDFrom(a.AttachCommand())
	fmt.Printf("session=%s window=%s\n", a.SessionID(), windowID)

	var stop atomic.Bool
	var lastFrame atomic.Value
	go func() {
		for i := 0; !stop.Load(); i++ {
			if frame, err := tmuxagent.CapturePane(windowID); err == nil {
				lastFrame.Store(frame)
				if *dump != "" {
					_ = os.WriteFile(filepath.Join(*dump, fmt.Sprintf("f%04d.txt", i)), frame, 0o644)
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()

	// Turn A: long enough that turn chrome is still on the pane for the
	// whole of B's submit loop (8 presses x 400ms plus paste settle).
	opener := *openerFlag
	fmt.Println("--- Send A (opener) ---")
	if err := a.Send(opener); err != nil {
		die("send opener: %v", err)
	}
	// Let the turn get going so the pane is unambiguously showing chrome.
	time.Sleep(*dwell)
	frameA, _ := lastFrame.Load().([]byte)
	fmt.Printf("pre-B frame: turn_chrome=%v ready=%v\n",
		tmuxagent.MatchTurnInProgress(frameA), tmuxagent.MatchReady(frameA))
	writeFrame(*dump, "A-turn-running.txt", frameA)

	// Turn B: the mid-turn payload. Large + multi-line → bracketed paste.
	payload := makePayload(*size)
	fmt.Printf("--- Send B (mid-turn, %d bytes) ---\n", len(payload))
	t0 := time.Now()
	sendErr := a.Send(payload)
	fmt.Printf("Send B returned after %s: err=%v\n", time.Since(t0), sendErr)
	writeFrame(*dump, "B-just-after-send.txt", frameOf(&lastFrame))

	// Watch the pane: did B ever leave the composer?
	deadline := time.Now().Add(*settle)
	cleared := false
	for time.Now().Before(deadline) {
		frame := frameOf(&lastFrame)
		if frame != nil && !composerHoldsPayload(frame) {
			cleared = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	final := frameOf(&lastFrame)
	stop.Store(true)
	time.Sleep(400 * time.Millisecond)
	writeFrame(*dump, "B-verdict.txt", final)

	fmt.Printf("\n=== VERDICT ===\nsend_err=%v\ncomposer_cleared=%v\n", sendErr, cleared)
	fmt.Printf("final frame: turn_chrome=%v ready=%v unsubmitted_paste=%v\n",
		tmuxagent.MatchTurnInProgress(final), tmuxagent.MatchReady(final),
		tmuxagent.MatchUnsubmittedPaste(final))
	fmt.Printf("---- final frame ----\n%s\n---------------------\n", final)
	switch {
	case sendErr == nil && !cleared:
		fmt.Println("REPRODUCED 🎯T28: Send reported success; the payload is still sitting in the composer.")
		os.Exit(1)
	case sendErr != nil && !cleared:
		fmt.Println("CONTRACT HELD: the payload never submitted and Send reported the failure.")
	case sendErr == nil && cleared:
		fmt.Println("CONTRACT HELD: the payload left the composer and Send reported success.")
	default:
		fmt.Println("OVER-BROAD: Send reported failure but the payload did submit.")
		os.Exit(2)
	}
}

// payloadMarker is a phrase unique to the probe payload, so "is it still
// in the composer" is decided by the pane, not by inference.
const payloadMarker = "T28MIDTURNPAYLOAD"

// composerRe extracts the live composer at the tail of the frame: the
// text between the last two horizontal rules. Anything above that is
// transcript, where the payload legitimately appears once submitted —
// grepping the whole frame would call a delivered payload "stuck".
var composerRe = regexp.MustCompile(`─{10,}\n❯([^\n]*(?:\n(?:[ \t\x{00A0}][^\n]*)?)*)\n─{10,}`)

// composerHoldsPayload reports whether the composer still visibly holds
// the probe payload. Claude Code soft-wraps the body and collapses big
// pastes into a chip, so both the marker and the chip count.
func composerHoldsPayload(frame []byte) bool {
	m := composerRe.FindAllSubmatch(frame, -1)
	if len(m) == 0 {
		return tmuxagent.MatchUnsubmittedPaste(frame)
	}
	body := m[len(m)-1][1]
	return strings.Contains(string(body), payloadMarker) ||
		strings.Contains(string(body), "[Pasted text #")
}

func makePayload(size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — mid-turn probe for T28. Reply with exactly T28PONG.\n\n", payloadMarker)
	line := "This paragraph pads the payload so the send path takes the bracketed-paste branch.\n"
	for b.Len() < size {
		b.WriteString(line)
	}
	fmt.Fprintf(&b, "\n%s — reply with exactly T28PONG and nothing else.\n", payloadMarker)
	return b.String()
}

func frameOf(v *atomic.Value) []byte {
	f, _ := v.Load().([]byte)
	return f
}

func writeFrame(dump, name string, frame []byte) {
	if dump == "" || frame == nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dump, name), frame, 0o644)
}

// windowIDFrom pulls the tmux target out of Agent.AttachCommand, which
// renders as `tmux -S <sock> attach -t <windowID>`. The Agent does not
// expose the window id directly.
func windowIDFrom(attach string) string {
	fields := strings.Fields(attach)
	for i, f := range fields {
		if f == "-t" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	die("cannot parse window id from %q", attach)
	return ""
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(3)
}
