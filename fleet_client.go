// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// ErrFleetUnsupported means the running broker predates fleets.
var ErrFleetUnsupported = errors.New("claudia: the running broker does not support fleets; restart it on a current release")

// fleetReadWait is how long one read waits on the broker before asking again.
const fleetReadWait = 30 * time.Second

// Fleet is a client's handle on a fleet the broker runs. It holds no connection
// and no state, so a client or broker can restart at any point: reopen the fleet
// by name and read from the last sequence number you saw.
type Fleet struct {
	name string
}

// OpenFleet creates the fleet if it is new, or attaches to it if not.
func OpenFleet(ctx context.Context, name string, opts *FleetOptions) (*Fleet, error) {
	f := &Fleet{name: name}
	req := &broker.FleetRequest{Op: broker.FleetOpOpen}
	if opts != nil {
		req.MaxAgents, req.MaxMessages = opts.MaxAgents, opts.MaxMessages
	}
	if _, err := f.call(ctx, req); err != nil {
		return nil, err
	}
	return f, nil
}

// Start launches a root agent.
func (f *Fleet) Start(ctx context.Context, args *FleetStartArgs) error {
	req := &broker.FleetRequest{
		Op: broker.FleetOpStart, Name: args.Name, Instructions: args.Instructions, InstructionsFile: args.InstructionsFile,
		WorkDir: args.WorkDir, Quality: args.Quality, Purpose: args.Purpose, Model: args.Model,
	}
	if args.Budget != nil {
		req.HasBudget, req.BudgetTokens, req.BudgetTurns, req.BudgetSeconds = true, args.Budget.Tokens, args.Budget.Turns, args.Budget.Seconds
	}
	_, err := f.call(ctx, req)
	return err
}

// FleetScheduleArgs is a message from the owner, scheduled.
type FleetScheduleArgs struct {
	To string
	// Kind is [FleetRequest], the default, or [FleetNotice].
	Kind  string
	Text  string
	After time.Duration
	At    time.Time
	Every time.Duration
}

// Schedule sends a message later, or repeatedly, and returns the timer ID that
// [Fleet.Cancel] takes.
func (f *Fleet) Schedule(ctx context.Context, args *FleetScheduleArgs) (string, error) {
	req := &broker.FleetRequest{Op: broker.FleetOpSend, To: args.To, Kind: args.Kind, Text: args.Text,
		AfterSeconds: args.After.Seconds(), EverySeconds: args.Every.Seconds()}
	if req.Kind == "" {
		req.Kind = FleetRequest
	}
	if !args.At.IsZero() {
		req.At = args.At.Format(time.RFC3339Nano)
	}
	resp, err := f.call(ctx, req)
	if err != nil {
		return "", err
	}
	var id string
	if err := json.Unmarshal(resp.Result, &id); err != nil {
		return "", fmt.Errorf("claudia: fleet schedule: %w", err)
	}
	return id, nil
}

// Cancel removes a scheduled message.
func (f *Fleet) Cancel(ctx context.Context, id string) error {
	_, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpCancel, TimerID: id})
	return err
}

// SetBudget sets the limits on the budget an agent holds, creating one over it
// and its descendants if it holds none.
func (f *Fleet) SetBudget(ctx context.Context, agent string, limits FleetBudget) error {
	_, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpBudget, Name: agent, HasBudget: true,
		BudgetTokens: limits.Tokens, BudgetTurns: limits.Turns, BudgetSeconds: limits.Seconds})
	return err
}

// RegisterCheck pins a check, replacing any of the same name. A ScriptFile is
// read here, and its contents sent.
func (f *Fleet) RegisterCheck(ctx context.Context, args *FleetCheckArgs) error {
	script := args.Script
	if args.ScriptFile != "" {
		raw, err := os.ReadFile(args.ScriptFile)
		if err != nil {
			return fmt.Errorf("claudia: read check script: %w", err)
		}
		script = string(raw)
	}
	_, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpRegisterCheck, Name: args.Name, Repo: args.Repo, Script: script,
		AgentsMayRun: args.AgentsMayRun, TimeoutSeconds: args.Timeout.Seconds()})
	return err
}

// RunCheck starts a run of a check against commit, empty for HEAD, and returns
// its ID. The result arrives as a [FleetEventCheckResult] event.
func (f *Fleet) RunCheck(ctx context.Context, name, commit string) (string, error) {
	resp, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpRunCheck, Name: name, Commit: commit})
	if err != nil {
		return "", err
	}
	var id string
	if err := json.Unmarshal(resp.Result, &id); err != nil {
		return "", fmt.Errorf("claudia: fleet run check: %w", err)
	}
	return id, nil
}

// Send sends a request whose response comes back as an event, and returns its
// ID. A positive delay holds it back that long.
func (f *Fleet) Send(ctx context.Context, to, text string, after time.Duration) (string, error) {
	resp, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpSend, To: to, Kind: FleetRequest, Text: text, AfterSeconds: after.Seconds()})
	if err != nil {
		return "", err
	}
	var id string
	if err := json.Unmarshal(resp.Result, &id); err != nil {
		return "", fmt.Errorf("claudia: fleet send: %w", err)
	}
	return id, nil
}

// Notify gives an agent news from source that needs no answer to source: the
// turn's text goes up the agent's parent tree.
func (f *Fleet) Notify(ctx context.Context, to, source, text string) error {
	_, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpSend, To: to, From: source, Kind: FleetNotice, Text: text})
	return err
}

// Stop ends one agent and returns without waiting for it to be gone.
func (f *Fleet) Stop(ctx context.Context, name string) error {
	_, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpStop, Name: name})
	return err
}

// StopTree stops everything under root and returns once it is gone.
func (f *Fleet) StopTree(ctx context.Context, root string) error {
	_, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpStopTree, Name: root})
	return err
}

// Shutdown stops every agent and deletes the fleet and its log.
func (f *Fleet) Shutdown(ctx context.Context) error {
	_, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpShutdown})
	return err
}

// Status reads the fleet as it is right now.
func (f *Fleet) Status(ctx context.Context) (*FleetStatus, error) {
	resp, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpStatus})
	if err != nil {
		return nil, err
	}
	var st FleetStatus
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		return nil, fmt.Errorf("claudia: fleet status: %w", err)
	}
	return &st, nil
}

// Read returns the events after seq, waiting until there are some or ctx ends.
// An error is usually a broker restarting: call again with the same seq.
func (f *Fleet) Read(ctx context.Context, after uint64) ([]FleetEvent, error) {
	for {
		resp, err := f.call(ctx, &broker.FleetRequest{Op: broker.FleetOpRead, After: after, WaitMillis: fleetReadWait.Milliseconds()})
		if err != nil {
			return nil, err
		}
		var events []FleetEvent
		if err := json.Unmarshal(resp.Result, &events); err != nil {
			return nil, fmt.Errorf("claudia: fleet read: %w", err)
		}
		if len(events) > 0 {
			return events, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

func (f *Fleet) call(ctx context.Context, req *broker.FleetRequest) (*broker.FleetResponse, error) {
	req.Fleet = f.name
	b, err := dialBroker()
	if err != nil {
		return nil, fmt.Errorf("claudia: fleet needs the broker: %w", err)
	}
	defer b.Close()
	resp, err := b.call(ctx, &broker.Request{Type: broker.TypeFleet, Fleet: req})
	if err != nil {
		var pe *broker.ProtocolError
		if errors.As(err, &pe) && pe.Code == broker.CodeUnknownType {
			return nil, ErrFleetUnsupported
		}
		return nil, err
	}
	if resp.Type != broker.TypeFleetResult || resp.Fleet == nil {
		return nil, fmt.Errorf("claudia: broker answered a fleet request with %s", resp.Type)
	}
	return resp.Fleet, nil
}
