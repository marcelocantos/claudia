// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// 🎯T30 acceptance 3 wants the large-payload claim proven on the REAL
// path — Agent.Send → claudeAgentOps().send → tmuxagent.SendKeys →
// waitContentLanded / ensureSubmitted, against a live Claude Code — not
// on captured frames. That costs a real turn per case, so it is gated on
// CLAUDIA_LIVE_SEND=1 and is residual verification: the committed
// oracles are the frame replays in internal/tmuxagent
// (send_t30_large_payload_test.go).
//
// The two verdicts it reads are independent, and 🎯T30 exists because
// they disagreed: what Send RETURNED, and whether the payload actually
// reached the model, read from the receiving session's own transcript.
// The transcript side must count the queued form too — a payload
// accepted behind a running turn is written as a queued_command
// attachment plus queue-operation records and never becomes a
// type:"user" record, so matching user messages alone reports a false
// "never delivered".
const t30LiveEnv = "CLAUDIA_LIVE_SEND"

// t30LiveBytes is the payload size under test. The report that provoked
// 🎯T30 was 5511 characters; the clause floor is 6000.
const t30LiveBytes = 6400

func TestT30LargePayloadSubmitsOnRealPath(t *testing.T) {
	if os.Getenv(t30LiveEnv) != "1" {
		t.Skipf("set %s=1 to run the live 🎯T30 large-payload send (costs real turns)", t30LiveEnv)
	}
	for _, tc := range []struct {
		name    string
		midturn bool
	}{
		{"idle_composer", false},
		{"mid_turn", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := Start(Config{Provider: ProviderClaude, WorkDir: t.TempDir()})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer a.Stop()

			marker := fmt.Sprintf("T30LIVE%d", time.Now().UnixNano()%1e9)
			payload := t30LivePayload(marker, t30LiveBytes)
			if len(payload) < 6000 {
				t.Fatalf("payload is %d bytes; the clause floor is 6000", len(payload))
			}

			if tc.midturn {
				// The daily population: the receiver is already running a
				// turn when the report arrives, so the payload must queue.
				if err := a.Send("Count from 1 to 400, printing one number per line and nothing else."); err != nil {
					t.Fatalf("busy prompt: %v", err)
				}
				if err := t30WaitTurnRunning(a); err != nil {
					t.Fatal(err)
				}
			}

			sendErr := a.Send(payload)
			d := t30WatchDelivery(t, a.JSONLPath(), marker, 90*time.Second)

			// The failure this target names is the pair (refused, delivered):
			// the payload reached the model and the caller was told it did
			// not, so a worker either splits its report by hand or its
			// evidence never arrives.
			if sendErr != nil {
				t.Fatalf("Send refused a %d-byte payload: %v\n  transcript says delivered=%v (user=%v attachment=%v enqueue=%v answered=%v)",
					len(payload), sendErr, d.delivered, d.userMessage, d.attachment, d.enqueue, d.answered)
			}
			if !d.delivered {
				t.Fatalf("Send reported success but the payload is absent from %s", a.JSONLPath())
			}
			if !d.answered {
				t.Errorf("payload delivered but no turn answered it within the wait; Send's success is unconfirmed")
			}
		})
	}
}

// t30LivePayload builds a payload of the requested size that names
// itself, so the transcript can be searched for this send specifically,
// and asks for a one-token reply so a delivered payload produces cheap,
// unambiguous evidence that a turn actually began.
func t30LivePayload(marker string, size int) string {
	head := fmt.Sprintf("%s — delivery probe for 🎯T30. Reply with exactly %s-ACK and nothing else.\n", marker, marker)
	var b strings.Builder
	b.WriteString(head)
	for b.Len() < size {
		b.WriteString("This paragraph pads the payload to the size a real finish report reaches. ")
	}
	return b.String()[:max(size, len(head))]
}

func t30WaitTurnRunning(a *Agent) error {
	windowID := t30WindowID(a.AttachCommand())
	for range 40 {
		frame, err := tmuxagent.CapturePane(windowID)
		if err == nil && tmuxagent.MatchTurnInProgress(frame) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("no turn chrome after the busy prompt; the mid-turn case never got a turn to queue behind")
}

// t30WindowID pulls the tmux target out of Agent.AttachCommand, which
// renders as `tmux -S <sock> attach -t <windowID>`.
func t30WindowID(attach string) string {
	fields := strings.Fields(attach)
	for i, f := range fields {
		if f == "-t" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

type t30Delivery struct {
	userMessage bool
	attachment  bool
	enqueue     bool
	answered    bool
	delivered   bool
}

func t30WatchDelivery(t *testing.T, path, marker string, timeout time.Duration) t30Delivery {
	t.Helper()
	var d t30Delivery
	deadline := time.Now().Add(timeout)
	for {
		d = t30ScanTranscript(path, marker)
		if d.answered || !time.Now().Before(deadline) {
			return d
		}
		time.Sleep(time.Second)
	}
}

func t30ScanTranscript(path, marker string) t30Delivery {
	var d t30Delivery
	raw, err := os.ReadFile(path)
	if err != nil {
		return d
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		typ, _ := rec["type"].(string)
		if typ == "assistant" && strings.Contains(line, marker+"-ACK") {
			d.answered = true
			continue
		}
		if !strings.Contains(line, marker) {
			continue
		}
		switch typ {
		case "user":
			d.userMessage = true
		case "attachment":
			d.attachment = true
		case "queue-operation":
			if op, _ := rec["operation"].(string); op == "enqueue" {
				d.enqueue = true
			}
		}
	}
	d.delivered = d.userMessage || d.attachment || d.enqueue
	return d
}
