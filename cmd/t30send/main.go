// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Command t30send answers the 🎯T30 question the size sweep cannot:
// what does the REAL send path — Agent.Send → tmuxagent.SendKeys →
// waitContentLanded → ensureSubmitted — do with a payload big enough to
// collapse into a Claude Code paste chip?
//
// cmd/t28size pastes and presses Enter itself, so it measures the CLI's
// behaviour with claudia's classifier removed. That is the wrong
// instrument for a failure whose error string ("composer state=paste_chip
// after 8 Enter presses") is produced by the classifier. This command
// drives the shipped path and records two things the pane alone cannot
// separate:
//
//   - what Send returned, and the frames the submit loop was looking at
//     while it decided (a recorder captures the pane throughout the send,
//     dumping every frame that differs from the last);
//   - whether the payload actually reached the model, read from the
//     receiving session's own transcript — including the queued form
//     (attachment + queue-operation records), which never becomes a
//     type:"user" record and so reads as "never delivered" to an oracle
//     that only matches user messages (claudia-po, 🎯T30 brief).
//
// A run costs one real turn per case, and with -midturn one more for the
// turn the case is pasted into.
//
// What it produced, kept as the 🎯T30 evidence: the pre-fix run of
// 2026-08-15 08:46 returned "ERROR turn not submitted: composer empty
// after paste (brief never reached pane)" for a 6400-byte payload the
// same run found in the receiver's transcript, answered. Two frames of
// that send are committed verbatim as
// internal/tmuxagent/testdata/frame_t30_chip_before_enter.txt and
// frame_t30_spinner_drained_after_enter.txt and are replayed by
// TestT30LiveReproSendSucceeds. The standing oracle for the fixed
// behaviour is TestT30LargePayloadSubmitsOnRealPath (root package,
// CLAUDIA_LIVE_SEND=1), which drives the same path from a test; this
// command remains the instrument for the next round of CLI chrome drift,
// because only it records what the submit loop was looking at.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

func main() {
	workdir := flag.String("workdir", ".", "workdir for the probe session")
	dump := flag.String("dump", "", "directory to write recorded frames into")
	bytesFlag := flag.Int("bytes", 6400, "payload size in bytes (🎯T30 clause 3 wants at least 6000)")
	lines := flag.Int("lines", 1, "spread the payload over this many lines")
	midturn := flag.Bool("midturn", false, "start a long-running turn first, so the payload is sent into a BUSY composer")
	interval := flag.Duration("interval", 150*time.Millisecond, "how often the recorder captures the pane during the send")
	deliverWait := flag.Duration("deliver-wait", 90*time.Second, "how long to watch the session transcript for the payload")
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
	fmt.Printf("session=%s window=%s jsonl=%s\n\n", a.SessionID(), windowID, a.JSONLPath())

	marker := fmt.Sprintf("T30SEND%d", time.Now().UnixNano()%1e9)
	payload := buildPayload(marker, *bytesFlag, *lines)
	fmt.Printf("marker=%s bytes=%d lines=%d midturn=%v\n", marker, len(payload), strings.Count(payload, "\n")+1, *midturn)

	if *midturn {
		if err := startBusyTurn(a, windowID); err != nil {
			die("busy turn: %v", err)
		}
		fmt.Println("busy turn running")
	}

	rec := recordFrames(windowID, *dump, *interval)
	start := time.Now()
	sendErr := a.Send(payload)
	elapsed := time.Since(start)
	frames := rec()

	fmt.Printf("\nsend returned after %s: ", elapsed.Round(10*time.Millisecond))
	if sendErr != nil {
		fmt.Printf("ERROR %v\n", sendErr)
	} else {
		fmt.Println("nil (reported submitted)")
	}
	fmt.Printf("recorded %d distinct frames during the send\n", frames)

	// The pane verdict and the delivery verdict are independent oracles,
	// and 🎯T30 exists because they disagree: a send reported as failed
	// may have been queued and consumed anyway.
	d := watchDelivery(a.JSONLPath(), marker, *deliverWait)
	fmt.Printf("\ndelivery (transcript %s):\n", filepath.Base(a.JSONLPath()))
	fmt.Printf("  user_message=%v attachment=%v queue_enqueue=%v queue_dequeue=%v first_seen=%s\n",
		d.userMessage, d.attachment, d.enqueue, d.dequeue, d.firstSeen)
	fmt.Printf("  assistant_replied=%v\n", d.assistantAfter)

	fmt.Printf("\nverdict: send=%s delivered=%v\n", verdictOf(sendErr), d.delivered())
}

// buildPayload builds a payload of the requested size that names itself,
// so both the pane and the transcript can be searched for this send
// specifically, and asks for a one-token reply so a delivered payload
// produces cheap, unambiguous evidence that a turn actually began.
func buildPayload(marker string, size, lines int) string {
	head := fmt.Sprintf("%s — delivery probe for 🎯T30. Reply with exactly %s-ACK and nothing else.\n", marker, marker)
	var b strings.Builder
	b.WriteString(head)
	filler := "This paragraph pads the payload to the size a real finish report reaches. "
	for b.Len() < size {
		b.WriteString(filler)
		if lines > 1 && strings.Count(b.String(), "\n") < lines-1 {
			b.WriteString("\n")
		}
	}
	return b.String()[:max(size, len(head))]
}

// busyPrompt starts a turn long enough to still be running while the
// payload is sent into it. It asks for output rather than thought, so
// the turn is reliably slow without depending on model latency.
const busyPrompt = "Count from 1 to 400, printing one number per line and nothing else."

func startBusyTurn(a *claudia.Agent, windowID string) error {
	if err := a.Send(busyPrompt); err != nil {
		return fmt.Errorf("send busy prompt: %w", err)
	}
	for range 40 {
		frame, err := tmuxagent.CapturePane(windowID)
		if err == nil && tmuxagent.MatchTurnInProgress(frame) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("no turn chrome after busy prompt")
}

// recordFrames captures the pane in the background for as long as the
// send runs, dumping every frame that differs from the one before it.
// These are the frames the submit loop classified, which is the only way
// to see WHY it decided what it decided. The returned func stops the
// recorder and reports how many distinct frames were kept.
func recordFrames(windowID, dump string, interval time.Duration) func() int {
	stop := make(chan struct{})
	done := make(chan int)
	go func() {
		var last string
		n := 0
		for {
			select {
			case <-stop:
				done <- n
				return
			default:
			}
			frame, err := tmuxagent.CapturePane(windowID)
			if err == nil && string(frame) != last {
				last = string(frame)
				if dump != "" {
					name := fmt.Sprintf("send_%02d_%s.txt", n, time.Now().Format("150405.000"))
					_ = os.WriteFile(filepath.Join(dump, name), frame, 0o644)
				}
				n++
			}
			time.Sleep(interval)
		}
	}()
	return func() int {
		close(stop)
		return <-done
	}
}

// deliveryVerdict records how the payload appears in the receiving
// session's transcript. The queued form is deliberately reported
// separately: a mid-turn payload is written as a queued_command
// attachment plus a pair of queue-operation records and never becomes a
// type:"user" record, so matching user messages alone reports a false
// "never delivered" (claudia-po, 🎯T30 brief).
type deliveryVerdict struct {
	userMessage    bool
	attachment     bool
	enqueue        bool
	dequeue        bool
	assistantAfter bool
	firstSeen      string
}

func (d deliveryVerdict) delivered() bool {
	return d.userMessage || d.attachment || d.enqueue
}

func watchDelivery(path, marker string, timeout time.Duration) deliveryVerdict {
	var d deliveryVerdict
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d = scanTranscript(path, marker)
		if d.assistantAfter {
			return d
		}
		time.Sleep(1 * time.Second)
	}
	return d
}

func scanTranscript(path, marker string) deliveryVerdict {
	var d deliveryVerdict
	raw, err := os.ReadFile(path)
	if err != nil {
		return d
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		typ, _ := rec["type"].(string)
		if typ == "assistant" && strings.Contains(line, marker+"-ACK") {
			d.assistantAfter = true
			continue
		}
		if !strings.Contains(line, marker) {
			continue
		}
		if d.firstSeen == "" {
			d.firstSeen, _ = rec["timestamp"].(string)
		}
		switch typ {
		case "user":
			d.userMessage = true
		case "attachment":
			d.attachment = true
		case "queue-operation":
			switch op, _ := rec["operation"].(string); op {
			case "enqueue":
				d.enqueue = true
			default:
				d.dequeue = true
			}
		}
	}
	return d
}

func verdictOf(err error) string {
	if err == nil {
		return "ok"
	}
	if strings.Contains(err.Error(), "turn not submitted") {
		return "refused"
	}
	return "error"
}

// windowIDFrom pulls the tmux target out of Agent.AttachCommand, which
// renders as `tmux -S <sock> attach -t <windowID>`.
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
