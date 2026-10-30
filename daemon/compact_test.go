// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	claudia "github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestT244BrokerCompactPropagatesConnectionCancel(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	var sawCtx context.Context
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		return claudia.StartStub(ctx, cfg, &claudia.StubAgentOps{
			Compact: func(ctx context.Context, _ int) (claudia.SeatCompact, error) {
				sawCtx = ctx
				close(started)
				<-ctx.Done()
				return claudia.SeatCompact{}, ctx.Err()
			},
		})
	}
	f.boot(t, nil)
	conn := rawSeat(t, f.sock, "compact-cancel")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = conn.WriteRequest(&broker.Request{ID: "compact", Type: broker.TypeCompact, Compact: &broker.CompactRequest{
			Name: "compact-cancel", BoundTokens: 180_000,
		}})
		for {
			if _, err := conn.ReadResponse(); err != nil {
				return
			}
		}
	}()
	waitFor(t, "compact started", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})
	_ = conn.Close()
	wg.Wait()
	waitFor(t, "compact ctx canceled", func() bool {
		return sawCtx != nil && sawCtx.Err() != nil
	})
	if !errors.Is(sawCtx.Err(), context.Canceled) {
		t.Fatalf("compact ctx err = %v, want context.Canceled after disconnect", sawCtx.Err())
	}
}
