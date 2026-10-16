// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func (d *Daemon) handleUsageFacts(c *broker.ClientConn, req *broker.Request) {
	q := req.UsageFacts
	page, err := claudia.QueryUsageDirect(d.ctx, claudia.UsageQuery{Seat: q.Seat, SessionID: q.SessionID, After: q.After, Limit: q.Limit})
	if err != nil {
		_ = c.Fail(req.ID, err)
		return
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeUsageFactsResult, UsageFacts: page})
}
