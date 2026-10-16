// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

type GenerationIdentity = omp.GenerationIdentity
type GenerationIssue = omp.GenerationIssue
type GenerationPage = omp.GenerationPage
type GenerationDiscoveryError = omp.GenerationDiscoveryError

const (
	GenerationPageLimit    = omp.GenerationPageLimit
	DiscoveryCursorExpired = omp.DiscoveryCursorExpired
	DiscoveryBusy          = omp.DiscoveryBusy
)

// GenerationQuery starts with an empty Cursor. Limit defaults to 128 and stays
// fixed throughout a scan. Cursors expire after 60 seconds idle or sidecar restart.
// On DiscoveryCursorExpired restart empty and deduplicate retained identities.
type GenerationQuery struct {
	Cursor string
	Limit  int
}

// DiscoverGenerations enumerates surviving canonical history headers, including
// unloaded/removed seats. No provider, credentials, or host ownership inference.
func DiscoverGenerations(ctx context.Context, q GenerationQuery) (*GenerationPage, error) {
	return discoverGenerations(ctx, q, false)
}

// DiscoverGenerationsDirect is the broker handler's non-recursive query seam.
func DiscoverGenerationsDirect(ctx context.Context, q GenerationQuery) (*GenerationPage, error) {
	return discoverGenerations(ctx, q, true)
}
func discoverGenerations(ctx context.Context, q GenerationQuery, direct bool) (*GenerationPage, error) {
	if q.Limit == 0 {
		q.Limit = GenerationPageLimit
	}
	if q.Limit < 1 || q.Limit > GenerationPageLimit || len(q.Cursor) > omp.GenerationCursorLimit {
		return nil, fmt.Errorf("invalid generation discovery query")
	}
	ctx, cancel := context.WithTimeout(ctx, receiptOpTimeout)
	defer cancel()
	if direct || !usingBroker() {
		return omp.DiscoverGenerations(ctx, q.Cursor, q.Limit)
	}
	b, err := dialBroker()
	if err != nil {
		return nil, err
	}
	defer b.Close()
	request := &broker.GenerationsRequest{Cursor: q.Cursor, Limit: q.Limit}
	if deadline, ok := ctx.Deadline(); ok {
		request.ExpiresAt = deadline.UnixMilli()
	}
	resp, err := b.call(ctx, &broker.Request{Type: broker.TypeGenerations, Generations: request})
	if err != nil {
		var pe *broker.ProtocolError
		if errors.As(err, &pe) && (string(pe.Code) == DiscoveryCursorExpired || string(pe.Code) == DiscoveryBusy) {
			return nil, &GenerationDiscoveryError{Code: string(pe.Code), Message: pe.Msg}
		}
		return nil, err
	}
	if resp.Type != broker.TypeGenerationsResult {
		return nil, fmt.Errorf("broker returned no generation page")
	}
	if err := resp.Generations.Validate(q.Cursor, q.Limit); err != nil {
		return nil, err
	}
	return resp.Generations, nil
}
