// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

// Receipt lookup is read-only and does not require ownership of a live grant.
// The local broker socket is the access boundary, as for grants/status reads.
const receiptLookupTimeout = 10 * time.Second

func (d *Daemon) handleReceipt(c *broker.ClientConn, req *broker.Request) {
	ctx, cancel := context.WithTimeout(d.ctx, receiptLookupTimeout)
	defer cancel()
	q := req.Receipt
	r, err := omp.QueryReceipt(ctx, q.Seat, q.SessionID, q.RequestID)
	if err != nil {
		_ = c.Fail(req.ID, err)
		return
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeReceiptResult, Receipt: r})
}
