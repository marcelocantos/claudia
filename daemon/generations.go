// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func (d *Daemon) handleGenerations(c *broker.ClientConn, req *broker.Request) {
	q := req.Generations
	ctx := d.ctx
	if q.ExpiresAt > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(q.ExpiresAt))
		defer cancel()
	}
	page, err := claudia.DiscoverGenerationsDirect(ctx, claudia.GenerationQuery{Cursor: q.Cursor, Limit: q.Limit})
	if err != nil {
		var de *claudia.GenerationDiscoveryError
		if errors.As(err, &de) {
			err = &broker.ProtocolError{Code: broker.ErrorCode(de.Code), Msg: de.Message}
		}
		_ = c.Fail(req.ID, err)
		return
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGenerationsResult, Generations: page})
}
