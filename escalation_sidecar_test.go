// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T138: exercise the actual socket pump. Queue consumption and abort drain
// are behavioral sidecar tests in seat-receipts.test.ts, run by make gate;
// matching source text here never proved either behavior.
func TestOMPSidecarReportsIdentifiedAbsorbed(t *testing.T) {
	_, _, _, peer, events := receiptPipe(t)
	replyReceipt(t, peer, omp.Event{Type: "absorbed", Text: "same text", RequestID: "B", TurnID: "turn-A"})
	ev := <-events
	if ev.Type != "progress" || ev.ProgressType != ProgressDeliveryAbsorbed || ev.Text != "same text" || ev.RequestID != "B" || ev.TurnID != "turn-A" {
		t.Fatalf("absorption lost its identity: %+v", ev)
	}
}

func TestOMPSidecarAnnouncesLegacyAcceptedPrompt(t *testing.T) {
	ctrl, _, _, peer, events := receiptPipe(t)
	replyReceipt(t, peer, omp.Event{Type: "accepted", TurnID: "legacy-turn"})
	ev := <-events
	if ev.Type != "progress" || ev.ProgressType != ProgressPromptAccepted || ev.TurnID != "legacy-turn" || !ctrl.inflight.Load() {
		t.Fatalf("legacy acceptance was not observable: %+v", ev)
	}
}
