// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"

	"github.com/marcelocantos/claudia/internal/broker"
)

// BrokerSeat is one seat as the running broker sees it (jevons 🎯T984).
type BrokerSeat struct {
	Name     string
	Provider Provider
	// Owned reports that a live connection holds the seat.
	Owned bool
	// Alive reports that the seat's provider process is reachable.
	Alive bool
}

// BrokerSeats lists every seat the running broker holds. A consumer whose own
// view of a seat can drift from the broker's (a handle it thinks dead while
// the broker resumed the seat) compares the two with this.
func BrokerSeats(ctx context.Context) ([]BrokerSeat, error) {
	client, err := dialBroker()
	if err != nil {
		return nil, fmt.Errorf("claudia: broker is required to list seats: %w", err)
	}
	defer client.Close()
	resp, err := client.call(ctx, &broker.Request{Type: broker.TypeGrants, Grants: &broker.GrantsRequest{}})
	if err != nil {
		return nil, err
	}
	if resp.Type != broker.TypeGrantsResult || resp.Grants == nil {
		return nil, fmt.Errorf("claudia: unexpected grants response %q", resp.Type)
	}
	out := make([]BrokerSeat, 0, len(resp.Grants.Grants))
	for _, g := range resp.Grants.Grants {
		out = append(out, BrokerSeat{Name: g.Name, Provider: Provider(g.Provider), Owned: g.Owned, Alive: g.Alive})
	}
	return out, nil
}

// StopBrokerSeat stops a seat no connection holds and drops its grant
// (jevons 🎯T984): a seat the broker resumed although its consumer meant it
// to stay down. A seat another connection holds is refused; its holder stops
// it.
func StopBrokerSeat(ctx context.Context, name string) error {
	client, err := dialBroker()
	if err != nil {
		return fmt.Errorf("claudia: broker is required to stop a seat: %w", err)
	}
	defer client.Close()
	resp, err := client.call(ctx, &broker.Request{Type: broker.TypeRelease,
		Release: &broker.ReleaseRequest{Name: name, Disposition: broker.DispositionStop}})
	if err != nil {
		return err
	}
	if resp.Type != broker.TypeReleased {
		return fmt.Errorf("claudia: unexpected release response %q", resp.Type)
	}
	return nil
}
