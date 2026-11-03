// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// fleetDirName holds the kernel's state under the broker's state directory.
const fleetDirName = "fleet"

// fleetDefaultQuality is the floor for an agent that names none.
const fleetDefaultQuality = "standard"

// openFleetKernel starts the kernel, which is dormant until a client opens a
// fleet. Damaged fleet state disables fleets and leaves the file alone. It does
// not stop the broker, whose other seats have nothing to do with it.
func (d *Daemon) openFleetKernel(stateDir string) {
	if d.mcp == nil {
		d.mu.Lock()
		d.fleetErr = errors.New("this broker runs without an MCP host, which fleets need")
		d.mu.Unlock()
		return
	}
	k, err := claudia.OpenFleetKernel(&claudia.FleetKernelArgs{
		Dir:    filepath.Join(stateDir, fleetDirName),
		Start:  d.startFleetSeat,
		Logger: func(msg string, args ...any) { d.log.Warn(msg, args...) },
	})
	if err != nil {
		d.log.Error("fleets disabled", "err", err)
		d.mu.Lock()
		d.fleetErr = err
		d.mu.Unlock()
		return
	}
	d.mcp.ServeLocal(claudia.FleetMCPServerName, k)
	d.mu.Lock()
	d.fleet = k
	d.mu.Unlock()
	k.Resume()
}

func (d *Daemon) fleetKernel() (*claudia.FleetKernel, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fleet == nil && d.fleetErr == nil {
		return nil, errors.New("the fleet kernel is still starting")
	}
	return d.fleet, d.fleetErr
}

func fleetGrantName(fleet, agent string) string { return "fleet-" + fleet + "-" + agent }

// startFleetSeat runs one agent. A fleet seat is an ordinary registered seat
// with no owning connection and no AutoStart: the kernel alone decides when it
// comes back, so a restart neither nudges it nor races the kernel to resume it.
func (d *Daemon) startFleetSeat(ctx context.Context, spec *claudia.FleetSeatSpec) (claudia.FleetSeat, error) {
	name := fleetGrantName(spec.Fleet, spec.Name)
	if deny, _ := d.consumerPolicyHold(name); deny {
		return nil, fmt.Errorf("the consumer's fleet policy holds %s", name)
	}
	ctx, cancel := context.WithTimeout(ctx, grantStartTimeout)
	defer cancel()

	existing := d.reg.Def(name)
	if existing != nil && !spec.Resume {
		// Left by a crash between a seat's end and the kernel's record of it.
		if err := d.reg.Remove(name); err != nil {
			return nil, fmt.Errorf("clear stale seat %s: %w", name, err)
		}
		existing = nil
	}
	if existing == nil {
		provider, model, err := d.pickFleetModel(spec)
		if err != nil {
			return nil, fmt.Errorf("no model satisfies the request: %w", err)
		}
		def := claudia.AgentDef{
			Name:         name,
			Provider:     provider,
			Model:        model,
			WorkDir:      spec.WorkDir,
			SessionID:    uuid.New().String(),
			Purpose:      claudia.PurposeWork,
			MCPExclusive: true,
			MCPServers:   []claudia.MCPServer{{Name: claudia.FleetMCPServerName, Type: "http", URL: d.mcp.LocalURL(claudia.FleetMCPServerName, spec.Token)}},
		}
		claudia.ApplyBrokerGrantWriteDefault(&def)
		if err := d.reg.Register(def); err != nil {
			return nil, err
		}
	}

	proc, err := d.reg.AdoptOrLaunchContext(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := proc.WaitReady(ctx); err != nil {
		_ = d.reg.Remove(name)
		return nil, err
	}

	s := &fleetSeat{d: d, name: name}
	s.last.Store(time.Now().UnixNano())
	proc.SubscribeEvents(func(claudia.Event) { s.last.Store(time.Now().UnixNano()) })
	d.mu.Lock()
	g := d.liveGrantLocked(name)
	if g == nil {
		g = &brokerGrant{name: name}
		d.grants[name] = g
	}
	if g.proc != proc {
		d.bindSeatLocked(g, proc)
	}
	d.mu.Unlock()
	d.emit(broker.EventMessage{Kind: broker.EventGrant, Name: name, SessionID: proc.SessionID(), Detail: "fleet"})
	return s, nil
}

func (d *Daemon) pickFleetModel(spec *claudia.FleetSeatSpec) (claudia.Provider, string, error) {
	if d.opts.fleetPick != nil {
		return d.opts.fleetPick(spec)
	}
	quality := spec.Quality
	if quality == "" {
		quality = fleetDefaultQuality
	}
	pick, err := d.resolvePick(claudia.ModelPredicates{
		Mode:       claudia.CapabilitySession,
		Quality:    claudia.ModelQuality(quality),
		Purpose:    claudia.ModelPurpose(spec.Purpose),
		Model:      spec.Model,
		PreferPlan: true,
	})
	if err != nil {
		return "", "", err
	}
	return pick.Provider, pick.Model, nil
}

// fleetSeat adapts a registered seat to the kernel. It looks the seat up on each
// use, because the registry can replace the process behind a name.
type fleetSeat struct {
	d    *Daemon
	name string
	last atomic.Int64
	once sync.Once
}

func (s *fleetSeat) proc() (*claudia.Agent, error) {
	proc := s.d.reg.Get(s.name)
	if proc == nil {
		return nil, fmt.Errorf("seat %s is not running", s.name)
	}
	return proc, nil
}

func (s *fleetSeat) Send(text string) error {
	proc, err := s.proc()
	if err != nil {
		return err
	}
	s.last.Store(time.Now().UnixNano())
	return proc.Send(text)
}

func (s *fleetSeat) WaitForResponse(ctx context.Context) (string, error) {
	proc, err := s.proc()
	if err != nil {
		return "", err
	}
	return proc.WaitForResponse(ctx)
}

func (s *fleetSeat) Describe() string {
	proc, err := s.proc()
	if err != nil {
		return "gone"
	}
	return fmt.Sprintf("%s/%s", proc.Provider(), proc.Model())
}

func (s *fleetSeat) Usage() any {
	proc, err := s.proc()
	if err != nil {
		return nil
	}
	return proc.Usage()
}

func (s *fleetSeat) LastActivity() time.Time { return time.Unix(0, s.last.Load()) }

// Stop ends the seat for good. It follows handleRelease's stop: interrupt a turn
// in progress and give it a moment to settle, then remove the seat regardless,
// because a stop stops (🎯T239).
func (s *fleetSeat) Stop() {
	s.once.Do(func() {
		d := s.d
		d.mu.Lock()
		g := d.liveGrantLocked(s.name)
		settle := d.releaseSettle
		d.mu.Unlock()
		if settle <= 0 {
			settle = defaultReleaseSettle
		}
		if proc := d.reg.Get(s.name); proc != nil && proc.PromptInFlight() {
			if err := proc.Quiesce(); err == nil {
				deadline := time.Now().Add(settle)
				for proc.PromptInFlight() && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
			}
		}
		d.mu.Lock()
		if g != nil {
			d.detachLocked(g)
			if g.proc != nil && g.sub != 0 {
				g.proc.UnsubscribeEvents(g.sub)
			}
			delete(d.grants, s.name)
		}
		d.mu.Unlock()
		d.standDown(s.name)
		if err := d.reg.Remove(s.name); err != nil {
			d.log.Warn("fleet seat stop", "grant", s.name, "err", err)
		}
		d.emit(broker.EventMessage{Kind: broker.EventRelease, Name: s.name})
	})
}

// handleFleet runs apart from the connection's read loop, because a start and a
// read can each take a long time.
func (d *Daemon) handleFleet(c *broker.ClientConn, req *broker.Request) {
	result, err := d.fleetOp(d.connectionContext(c), req.Fleet)
	if err != nil {
		var pe *broker.ProtocolError
		if !errors.As(err, &pe) {
			pe = &broker.ProtocolError{Code: broker.CodeUnsupportedValue, Field: "fleet", Value: req.Fleet.Fleet, Msg: err.Error()}
		}
		_ = c.Fail(req.ID, pe)
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeMalformed, Field: "fleet", Msg: err.Error()})
		return
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeFleetResult, Fleet: &broker.FleetResponse{Result: raw}})
}

func (d *Daemon) fleetOp(ctx context.Context, r *broker.FleetRequest) (any, error) {
	k, err := d.fleetKernel()
	if err != nil {
		return nil, err
	}
	switch r.Op {
	case broker.FleetOpOpen:
		return nil, k.Open(r.Fleet, &claudia.FleetOptions{MaxAgents: r.MaxAgents, MaxMessages: r.MaxMessages})
	case broker.FleetOpStart:
		return nil, k.Start(ctx, r.Fleet, claudia.FleetOwner, &claudia.FleetStartArgs{
			Name: r.Name, Instructions: r.Instructions, InstructionsFile: r.InstructionsFile,
			WorkDir: r.WorkDir, Quality: r.Quality, Purpose: r.Purpose, Model: r.Model,
		})
	case broker.FleetOpSend:
		from := claudia.FleetOwner
		if r.Kind == claudia.FleetNotice && r.From != "" {
			from = r.From
		}
		return k.Send(r.Fleet, from, r.To, r.Kind, r.Text, time.Duration(r.AfterSeconds*float64(time.Second)))
	case broker.FleetOpStop:
		return nil, k.Stop(r.Fleet, claudia.FleetOwner, r.Name)
	case broker.FleetOpStopTree:
		return nil, k.StopTree(ctx, r.Fleet, r.Name)
	case broker.FleetOpShutdown:
		return nil, k.Shutdown(ctx, r.Fleet)
	case broker.FleetOpStatus:
		return k.Status(r.Fleet)
	case broker.FleetOpRead:
		wait, cancel := context.WithTimeout(ctx, time.Duration(r.WaitMillis)*time.Millisecond)
		defer cancel()
		return k.Read(wait, r.Fleet, r.After)
	default:
		return nil, fmt.Errorf("unknown fleet operation %q", r.Op)
	}
}
