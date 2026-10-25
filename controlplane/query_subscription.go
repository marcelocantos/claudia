// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

type querySubscription struct {
	query   string
	id      sqlpipe.SubscriptionID
	updates chan sqlpipe.QueryResult
	stop    func() bool
}

// SubscribeQuery registers one sqldeep/SQL query. The first result is the
// persisted cache, even offline. Live results come from sqlpipe subscriptions,
// including initial synchronization and reconnect; local Write commits also
// reevaluate the query. Slow readers receive the latest complete result.
// Cancellation and Close close the channel. Callers own each delivered result.
func (r *Replica) SubscribeQuery(ctx context.Context, query string) (<-chan sqlpipe.QueryResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.db == nil || r.closing {
		return nil, fmt.Errorf("control plane: replica closed")
	}
	cached, err := r.db.Query(query)
	if err != nil {
		return nil, err
	}
	sub := &querySubscription{query: query, updates: make(chan sqlpipe.QueryResult, 1)}
	if r.peer != nil {
		sub.id, err = r.peer.Subscribe(query)
		if err != nil {
			return nil, err
		}
	}
	r.queries[sub] = struct{}{}
	sub.updates <- cached
	sub.stop = context.AfterFunc(ctx, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.queries[sub]; !ok {
			return
		}
		if r.peer != nil && sub.id != 0 {
			_ = r.peer.Unsubscribe(sub.id)
		}
		delete(r.queries, sub)
		close(sub.updates)
	})
	return sub.updates, nil
}

func (r *Replica) deliverQueryResults(results []sqlpipe.QueryResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, result := range results {
		for sub := range r.queries {
			if sub.id == result.ID {
				sub.deliver(result)
			}
		}
	}
}

func (sub *querySubscription) deliver(result sqlpipe.QueryResult) {
	select {
	case <-sub.updates:
	default:
	}
	sub.updates <- result
}

func (r *Replica) refreshQueriesLocked() error {
	for sub := range r.queries {
		result, err := r.db.Query(sub.query)
		if err != nil {
			return err
		}
		sub.deliver(result)
	}
	return nil
}
