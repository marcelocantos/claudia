// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"strings"
	"testing"
)

// 🎯T138: the sidecar reports when the model takes a message queued or
// steered behind a turn, and a hard-stop delivers what is queued at once
// rather than leaving it for some later prompt.
func TestOMPSidecarReportsAbsorbedAndDrainsOnAbort(t *testing.T) {
	raw, err := os.ReadFile("sidecar/seat.ts")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, want := range []string{
		`event.type === "message_start" && event.message?.role === "user"`,
		`sink.emit({ type: "absorbed", text });`,
		`pending.push(text);`,
		`void agent.waitForIdle().then(async () => {`,
		`await runTurn("", { cause: "interrupt-deliver" }, () => agent.continue());`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("seat.ts lacks %s", want)
		}
	}
	if n := strings.Count(src, "pending.push(text);"); n != 2 {
		t.Errorf("seat.ts tracks %d queue paths as pending; want the follow-up and the steer", n)
	}
	goSrc, err := os.ReadFile("omp_agent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goSrc), `a.publishEvent(Event{Type: "progress", ProgressType: ProgressDeliveryAbsorbed, Text: ev.Text})`) {
		t.Error("the sidecar's absorbed event must publish as delivery_absorbed")
	}
}
