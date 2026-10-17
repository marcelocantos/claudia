// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"log/slog"
	"sync"
)

// Observers publish sidecar usage facts and request receipts into the
// Claudia-owned control-plane file. The seat event pump calls
// RecordObservedUsage and RecordObservedReceipt after it accepts the
// event's identity. A nil observer leaves that stream unrecorded, and a
// write error does not drop the event from the seat.
var (
	controlObserveMu      sync.Mutex
	controlObserveUsage   func(*UsageFact) error
	controlObserveReceipt func(*RequestReceipt) error
)

// SetControlPlaneObservers installs the process publisher. Pass nil to
// clear one stream.
func SetControlPlaneObservers(usage func(*UsageFact) error, receipt func(*RequestReceipt) error) {
	controlObserveMu.Lock()
	controlObserveUsage = usage
	controlObserveReceipt = receipt
	controlObserveMu.Unlock()
}

// RecordObservedUsage publishes one accepted usage event.
func RecordObservedUsage(fact *UsageFact) {
	if fact == nil {
		return
	}
	controlObserveMu.Lock()
	fn := controlObserveUsage
	controlObserveMu.Unlock()
	if fn == nil {
		return
	}
	if err := fn(fact); err != nil {
		slog.Warn("control plane usage not recorded", "seat", fact.Seat, "session", fact.SessionID, "seq", fact.Seq, "err", err)
	}
}

// RecordObservedReceipt publishes one accepted request receipt.
func RecordObservedReceipt(receipt *RequestReceipt) {
	if receipt == nil {
		return
	}
	controlObserveMu.Lock()
	fn := controlObserveReceipt
	controlObserveMu.Unlock()
	if fn == nil {
		return
	}
	if err := fn(receipt); err != nil {
		slog.Warn("control plane receipt not recorded", "seat", receipt.Seat, "session", receipt.SessionID, "request", receipt.RequestID, "err", err)
	}
}
