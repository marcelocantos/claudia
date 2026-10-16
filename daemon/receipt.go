// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// Receipt lookup is read-only and does not require ownership of a live grant.
// The local broker socket is the access boundary, as for grants/status reads.
func (d *Daemon) handleReceipt(c *broker.ClientConn, req *broker.Request) {
	q := req.Receipt
	r, err := claudia.QueryReceiptDirect(d.ctx, claudia.ReceiptQuery{Seat: q.Seat, SessionID: q.SessionID, RequestID: q.RequestID})
	if err != nil {
		_ = c.Fail(req.ID, err)
		return
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeReceiptResult, Receipt: r})
}
