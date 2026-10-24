// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/marcelocantos/claudia/omp"
)

func (c *ompControl) callReceipt(ctx context.Context, msg omp.Message) (omp.Event, error) {
	if err := ctx.Err(); err != nil {
		return omp.Event{}, err
	}
	msg.RPCID = uuid.NewString()
	reply := make(chan omp.Event, 1)
	c.mu.Lock()
	if c.waits == nil {
		c.waits = map[string]chan omp.Event{}
	}
	if c.done == nil {
		c.done = make(chan struct{})
	}
	c.waits["rpc:"+msg.RPCID] = reply
	done := c.done
	c.mu.Unlock()
	defer c.takeWait("rpc:" + msg.RPCID)
	err := c.sendContext(ctx, msg)
	if err != nil {
		return omp.Event{}, err
	}
	select {
	case ev := <-reply:
		if ev.Type == "error" {
			return ev, fmt.Errorf("sidecar: %s", ev.Text)
		}
		return ev, nil
	case <-ctx.Done():
		return omp.Event{}, ctx.Err()
	case <-done:
		return omp.Event{}, fmt.Errorf("sidecar disconnected before receipt")
	}
}

func (c *ompControl) sendRequest(msg omp.Message, requestID string) error {
	if requestID == "" {
		return c.send(msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), receiptOpTimeout)
	defer cancel()
	return c.sendIdentified(ctx, msg, requestID)
}

func (c *ompControl) sendIdentified(ctx context.Context, msg omp.Message, requestID string) error {
	msg.RequestID, msg.AttemptID, msg.SessionID = requestID, uuid.NewString(), c.sessionID
	ev, err := c.callReceipt(ctx, msg)
	if err == nil && ev.Type != "accepted" {
		err = fmt.Errorf("expected durable accepted, got %q", ev.Type)
	}
	if err == nil {
		err = ev.Receipt.ValidateIdentity(c.seat, c.sessionID, requestID)
	}
	if err == nil && ev.Receipt.State == ReceiptUnknown {
		err = fmt.Errorf("request remains unknown: %s", ev.Receipt.Reason)
	}
	if err != nil {
		return fmt.Errorf("%w: request %q on seat %q session %q; query that original ID before any retry: %w", ErrReceiptUncertain, requestID, c.seat, c.sessionID, err)
	}
	return nil
}

func (c *ompControl) receipt(ctx context.Context, requestID string) (*RequestReceipt, error) {
	ev, err := c.callReceipt(ctx, omp.Message{Op: omp.OpReceipt, Seat: c.seat, SessionID: c.sessionID, RequestID: requestID})
	if err != nil {
		return nil, err
	}
	if ev.Type != "receipt" {
		return nil, fmt.Errorf("expected receipt, got %q", ev.Type)
	}
	if err := ev.Receipt.ValidateIdentity(c.seat, c.sessionID, requestID); err != nil {
		return nil, err
	}
	return ev.Receipt, nil
}
