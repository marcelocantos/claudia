// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

// SupportsReceipts reports whether identified submissions wait for durable
// sidecar admission and expose per-request execution receipts.
func (a *Agent) SupportsReceipts() bool {
	return a != nil && a.ops.explicitRequestIDs && a.ops.receipt != nil
}

type RequestReceipt = omp.RequestReceipt
type ReceiptState = omp.ReceiptState

const (
	ReceiptAccepted  = omp.ReceiptAccepted
	ReceiptStarted   = omp.ReceiptStarted
	ReceiptCompleted = omp.ReceiptCompleted
	ReceiptCancelled = omp.ReceiptCancelled
	ReceiptFailed    = omp.ReceiptFailed
	ReceiptUnknown   = omp.ReceiptUnknown
)

type ReceiptCause = omp.ReceiptCause

const (
	ReceiptCauseNone         = omp.ReceiptCauseNone
	ReceiptCauseAuth         = omp.ReceiptCauseAuth
	ReceiptCauseCapacity     = omp.ReceiptCauseCapacity
	ReceiptCauseCancellation = omp.ReceiptCauseCancellation
	ReceiptCauseFailure      = omp.ReceiptCauseFailure
	ReceiptCauseUnknown      = omp.ReceiptCauseUnknown
)

// ErrReceiptUncertain means acceptance cannot be established. Query the same
// request ID; neither this error nor a not_found receipt authorises replay.
var ErrReceiptUncertain = errors.New("request receipt uncertain")

// receiptOpTimeout bounds a local journal RPC, never model execution.
const receiptOpTimeout = 10 * time.Second

type ReceiptQuery struct {
	Seat      string
	SessionID string
	RequestID string
}

// Receipt reads this handle's durable request state, even when its model is
// idle. Backend support is explicit; no transcript or turn inference is used.
func (a *Agent) Receipt(ctx context.Context, requestID string) (*RequestReceipt, error) {
	if a == nil || strings.TrimSpace(requestID) == "" {
		return nil, fmt.Errorf("%w: missing agent or request ID", ErrReceiptUncertain)
	}
	if a.ops.receipt == nil {
		return nil, fmt.Errorf("%w: backend has no durable receipt API", ErrReceiptUncertain)
	}
	ctx, cancel := context.WithTimeout(ctx, receiptOpTimeout)
	defer cancel()
	r, err := a.ops.receipt(ctx, a, requestID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReceiptUncertain, err)
	}
	return r, nil
}

// QueryReceipt reads a named durable session, including an unloaded one.
// It never spawns a process, loads a model, refreshes auth, or retries input.
func QueryReceipt(ctx context.Context, q ReceiptQuery) (*RequestReceipt, error) {
	return queryReceipt(ctx, q, false)
}

// QueryReceiptDirect is QueryReceipt against the sidecar even when a broker is
// configured. The broker uses it to serve receipt reads without routing the
// request back through itself. It never loads a seat or starts the sidecar.
func QueryReceiptDirect(ctx context.Context, q ReceiptQuery) (*RequestReceipt, error) {
	return queryReceipt(ctx, q, true)
}

func queryReceipt(ctx context.Context, q ReceiptQuery, direct bool) (*RequestReceipt, error) {
	if strings.TrimSpace(q.Seat) == "" || strings.TrimSpace(q.SessionID) == "" || strings.TrimSpace(q.RequestID) == "" {
		return nil, fmt.Errorf("%w: seat, session and request ID are required", ErrReceiptUncertain)
	}
	ctx, cancel := context.WithTimeout(ctx, receiptOpTimeout)
	defer cancel()
	var r *RequestReceipt
	var err error
	if !direct && usingBroker() {
		var b *brokerClient
		b, err = dialBroker()
		if err == nil {
			defer b.Close()
			r, err = queryBrokerReceipt(ctx, b, q)
		}
	} else {
		r, err = omp.QueryReceipt(ctx, q.Seat, q.SessionID, q.RequestID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReceiptUncertain, err)
	}
	return r, nil
}

func queryBrokerReceipt(ctx context.Context, b *brokerClient, q ReceiptQuery) (*RequestReceipt, error) {
	resp, err := b.call(ctx, &broker.Request{Type: broker.TypeReceipt, Receipt: &broker.ReceiptRequest{Seat: q.Seat, SessionID: q.SessionID, RequestID: q.RequestID}})
	if err != nil {
		return nil, err
	}
	if resp.Type != broker.TypeReceiptResult || resp.Receipt == nil {
		return nil, fmt.Errorf("broker returned no durable receipt")
	}
	if err := resp.Receipt.ValidateIdentity(q.Seat, q.SessionID, q.RequestID); err != nil {
		return nil, err
	}
	return resp.Receipt, nil
}
