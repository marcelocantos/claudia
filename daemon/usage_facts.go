// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func (d *Daemon) handleUsageFacts(c *broker.ClientConn, req *broker.Request) {
	q := req.UsageFacts
	ctx := d.ctx
	if q.ExpiresAt > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(q.ExpiresAt))
		defer cancel()
	}
	page, err := claudia.QueryUsageDirect(ctx, claudia.UsageQuery{Seat: q.Seat, SessionID: q.SessionID, After: q.After, Limit: q.Limit})
	if err != nil {
		_ = c.Fail(req.ID, err)
		return
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeUsageFactsResult, UsageFacts: page})
}
