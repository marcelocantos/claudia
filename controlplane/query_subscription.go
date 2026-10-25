// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

type querySubscription struct {
	updates chan sqlpipe.QueryResult
	stop    func() bool
}

// SubscribeQuery registers one database-lifetime native sqldeep/SQL watch.
// Initial cached state is available offline. Native dependency filtering and
// result hashes govern both local commits and replicated changes. Slow readers
// receive the latest complete result. Cancellation and Close close the channel.
func (r *Replica) SubscribeQuery(ctx context.Context, query string) (<-chan sqlpipe.QueryResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.db == nil || r.closing {
		return nil, fmt.Errorf("control plane: replica closed")
	}
	if r.watch == nil {
		watch, err := sqlpipe.NewQueryWatch(r.db)
		if err != nil {
			return nil, err
		}
		r.watch = watch
	}
	initial, err := r.watch.Subscribe(query)
	if err != nil {
		return nil, err
	}
	id := initial.ID
	sub := &querySubscription{updates: make(chan sqlpipe.QueryResult, 1)}
	sub.updates <- initial
	r.queries[id] = sub
	sub.stop = context.AfterFunc(ctx, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.queries[id]; !ok {
			return
		}
		r.watch.Unsubscribe(id)
		delete(r.queries, id)
		close(sub.updates)
	})
	return sub.updates, nil
}

func (r *Replica) notifyQueriesLocked() error {
	if r.watch == nil {
		return nil
	}
	results, err := r.watch.Notify()
	if err != nil {
		return err
	}
	for _, result := range results {
		if sub, ok := r.queries[result.ID]; ok {
			select {
			case <-sub.updates:
			default:
			}
			sub.updates <- result
		}
	}
	return nil
}
