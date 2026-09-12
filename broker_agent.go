// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// The broker-backed Session backend (🎯T2.10 / 🎯T3). It satisfies
// agentBackend like every provider backend, so Start builds the same *Agent
// it always does; the difference is that every op is a request on the
// grant's connection and every Event arrives on that connection's push
// stream. The daemon runs the real provider backend, the JSONL tail, the
// readiness detector and the goal loop. This handle is a client of one seat.

// grantHint carries the Registry's adopt/fallback intent to the backend
// without widening Config.
type grantHint struct {
	adopt    bool
	fallback bool
	// def is the consumer Registry's full definition (purpose, parent,
	// role, target) so the daemon's table shows the seat as the consumer
	// knows it. Nil for a plain Start.
	def *AgentDef
}

type grantHintKey struct{}

// withGrantHint attaches adopt/fallback intent for startViaBroker.
func withGrantHint(ctx context.Context, h grantHint) context.Context {
	return context.WithValue(ctx, grantHintKey{}, h)
}

func grantHintFrom(ctx context.Context) grantHint {
	h, _ := ctx.Value(grantHintKey{}).(grantHint)
	return h
}

// grantStartTimeout bounds a grant round trip. Claude readiness detection
// alone is allowed 30s (readyOverallTimeout); adopt-then-launch can pay it
// twice.
const grantStartTimeout = 90 * time.Second

// brokerOpTimeout bounds one ordinary op (send, interrupt, set_model, …).
// Send on the daemon blocks on readiness, so this is not short.
const brokerOpTimeout = 60 * time.Second

// brokerAgentBackend is one grant on one connection.
type brokerAgentBackend struct {
	client *brokerClient
	cfg    Config // the daemon-side config (goal, term log, MCP intact)
	hint   grantHint

	mu     sync.Mutex
	agent  *Agent
	name   string
	attach string
	gone   bool

	// queue keeps pushed messages in arrival order until the Agent handle
	// exists (ready closes in DetectReady), then delivers them in that
	// order. A replayed reclaim history must not overtake live events.
	queue chan *broker.Response
	ready chan struct{}
	// subscribed closes on the consumer's first SubscribeEvents.
	subscribed chan struct{}
	subOnce    sync.Once
}

// replayGrace is how long a reclaim's replayed history waits for the
// consumer to attach a subscriber before it is delivered anyway.
const replayGrace = 500 * time.Millisecond

// Capabilities reports the provider's own matrix: a brokered seat can do
// what its provider can do.
func (b *brokerAgentBackend) Capabilities() providerCapabilities {
	return agentBackendForProvider(b.cfg.Provider).Capabilities()
}

// grantNameFor is the daemon key for a seat.
func grantNameFor(cfg Config, sessionID string) string {
	if cfg.Name != "" {
		return cfg.Name
	}
	if sessionID != "" {
		return "sid:" + sessionID
	}
	return "anon:" + newRunID()
}

// StartAgent sends the grant and wires the push stream.
func (b *brokerAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	cfg := b.cfg
	cfg.SessionID = req.SessionID
	cfg.WorkDir = req.WorkDir
	cfg.RequireResume = req.Config.RequireResume
	name := grantNameFor(cfg, req.SessionID)
	def, err := encodeGrantDefWire(configToGrantDef(name, cfg, b.hint.def))
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.name = name
	b.mu.Unlock()

	// Push handling is installed before the grant so a reclaim's replayed
	// events cannot race the response.
	b.queue = make(chan *broker.Response, brokerClientQueue)
	b.ready = make(chan struct{})
	b.subscribed = make(chan struct{})
	b.client.setPush(b.onPush)
	go b.drain()

	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, grantStartTimeout)
	defer cancel()
	resp, err := b.client.call(ctx, &broker.Request{
		Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{
			Name: name, Def: def, Adopt: b.hint.adopt, Fallback: b.hint.fallback,
		},
	})
	if err != nil {
		return nil, err
	}
	g := resp.Granted
	if g == nil {
		return nil, fmt.Errorf("broker: grant %s answered with %s", name, resp.Type)
	}
	b.mu.Lock()
	b.attach = g.AttachCommand
	b.mu.Unlock()
	if g.Lagged {
		slog.Warn("broker grant reclaimed with a lagged replay; event history is incomplete",
			"grant", name, "replayed", g.Replayed)
	}
	return &agentStart{
		WindowID:    g.WindowID,
		Ops:         b.ops(),
		SessionID:   g.SessionID,
		JSONLPath:   g.JSONLPath,
		TermLogPath: g.TermLogPath,
		ConnectURL:  g.ConnectURL,
		ConnectPID:  g.ConnectPID,
		GrantName:   name,
		// DetectReady only records the handle: readiness is the daemon's
		// (its Send blocks on it), so this handle is ready at once.
		DetectReady: func(a *Agent) {
			b.mu.Lock()
			b.agent = a
			b.mu.Unlock()
			a.mu.Lock()
			a.onSubscribe = func() { b.subOnce.Do(func() { close(b.subscribed) }) }
			a.mu.Unlock()
			close(b.ready)
			close(a.ready)
		},
		Cleanup: func() { b.client.Close() },
	}, nil
}

func (b *brokerAgentBackend) named() *broker.NamedRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &broker.NamedRequest{Name: b.name}
}

func (b *brokerAgentBackend) opCall(req *broker.Request) (*broker.Response, error) {
	return b.client.callTimeout(req, brokerOpTimeout)
}

func (b *brokerAgentBackend) ops() agentOps {
	return agentOps{
		attachCommand: func(*Agent) string {
			b.mu.Lock()
			defer b.mu.Unlock()
			return b.attach
		},
		send: func(_ *Agent, msg string) error {
			_, err := b.opCall(&broker.Request{Type: broker.TypeSend, Send: &broker.SendRequest{Name: b.named().Name, Text: msg}})
			return err
		},
		interrupt: func(*Agent) error {
			_, err := b.opCall(&broker.Request{Type: broker.TypeInterrupt, Interrupt: b.named()})
			return err
		},
		resize: func(_ *Agent, cols, rows uint16) error {
			_, err := b.opCall(&broker.Request{Type: broker.TypeResize, Resize: &broker.ResizeRequest{Name: b.named().Name, Cols: cols, Rows: rows}})
			return err
		},
		stop: func(*Agent) {
			_, err := b.opCall(&broker.Request{Type: broker.TypeRelease,
				Release: &broker.ReleaseRequest{Name: b.named().Name, Disposition: broker.DispositionStop}})
			if err != nil && !errors.Is(err, errBrokerClosed) {
				slog.Warn("broker release failed", "grant", b.named().Name, "err", err)
			}
			b.client.Close()
		},
		promptInFlight: func(*Agent) bool {
			resp, err := b.opCall(&broker.Request{Type: broker.TypeAgentInfo, AgentInfo: b.named()})
			if err != nil || resp.AgentInfo == nil {
				return false
			}
			return resp.AgentInfo.PromptInFlight
		},
		setModel: func(_ *Agent, model string) error {
			_, err := b.opCall(&broker.Request{Type: broker.TypeSetModel, SetModel: &broker.SetModelRequest{Name: b.named().Name, Model: model}})
			return err
		},
		migrate: b.migrate,
		closeGoal: func(*Agent) {
			if _, err := b.opCall(&broker.Request{Type: broker.TypeCloseGoal, CloseGoal: b.named()}); err != nil {
				slog.Warn("broker close_goal failed", "grant", b.named().Name, "err", err)
			}
		},
		subscribeTerminal: func(a *Agent) {
			resp, err := b.opCall(&broker.Request{Type: broker.TypeTermSubscribe, TermSubscribe: b.named()})
			if err != nil {
				slog.Warn("broker term_subscribe failed", "grant", b.named().Name, "err", err)
				return
			}
			if resp.TermSubscribed != nil && len(resp.TermSubscribed.History) > 0 {
				a.pushTermOutput(resp.TermSubscribed.History)
			}
		},
	}
}

// migrate asks the daemon to swap providers, then re-points this handle at
// the destination. The daemon's model_switch Event arrives on the stream.
func (b *brokerAgentBackend) migrate(a *Agent, args *MigrateArgs) error {
	ctx, cancel := context.WithTimeout(context.Background(), grantStartTimeout)
	defer cancel()
	resp, err := b.client.call(ctx, &broker.Request{Type: broker.TypeMigrate, Migrate: &broker.MigrateRequest{
		Name: b.named().Name, Provider: broker.Provider(args.Provider), Model: args.Model, Reason: args.Reason, Force: args.Force,
	}})
	if err != nil {
		return err
	}
	m := resp.Migrated
	if m == nil {
		return fmt.Errorf("broker: migrate answered with %s", resp.Type)
	}
	b.mu.Lock()
	b.attach = m.AttachCommand
	b.mu.Unlock()
	a.mu.Lock()
	a.backendGen.Add(1)
	a.provider = Provider(m.Provider)
	a.sessionID = m.SessionID
	a.jsonlPath = m.JSONLPath
	a.tmuxWindowID = m.WindowID
	if m.Model != "" {
		a.model = m.Model
	}
	a.mu.Unlock()
	a.termMu.Lock()
	if m.TermLogPath != "" {
		a.termLogPath = m.TermLogPath
		a.termLogLive = true
	} else {
		a.termLogLive = false
	}
	a.termMu.Unlock()
	return nil
}

// brokerClientQueue bounds pushed messages waiting for the handle. It is
// larger than the daemon's replay ring so a reclaim can never block the
// read loop before the grant response has been read.
const brokerClientQueue = 4096

// onPush queues id-less messages in arrival order.
func (b *brokerAgentBackend) onPush(resp *broker.Response) {
	select {
	case b.queue <- resp:
	case <-b.client.done:
	}
}

// drain delivers queued messages once the handle exists, and marks the
// handle unreachable when the connection is lost: a consumer that sees
// Alive() false re-grants by name, which is how it reconnects to a daemon
// that restarted (🎯T2.11).
func (b *brokerAgentBackend) drain() {
	select {
	case <-b.ready:
	case <-b.client.done:
		return
	}
	b.mu.Lock()
	a := b.agent
	b.mu.Unlock()
	// Replayed history waits for the consumer's first subscriber, up to
	// the grace, so a Start-then-Subscribe consumer does not miss it.
	select {
	case <-b.subscribed:
	case <-time.After(replayGrace):
	case <-b.client.done:
	}
	for {
		select {
		case resp := <-b.queue:
			b.deliver(a, resp)
		case <-b.client.done:
			for {
				select {
				case resp := <-b.queue:
					b.deliver(a, resp)
				default:
					a.mu.Lock()
					a.alive = false
					a.mu.Unlock()
					return
				}
			}
		}
	}
}

func (b *brokerAgentBackend) deliver(a *Agent, resp *broker.Response) {
	switch resp.Type {
	case broker.TypeAgentEvent:
		ev, err := decodeEventWire(resp.AgentEvent.Event)
		if err != nil {
			slog.Warn("broker agent event undecodable", "grant", b.named().Name, "err", err)
			return
		}
		a.publishEvent(ev)
	case broker.TypeAgentTerm:
		a.pushTermOutput(resp.AgentTerm.Data)
	case broker.TypeAgentGone:
		b.mu.Lock()
		b.gone = true
		b.mu.Unlock()
		a.mu.Lock()
		a.alive = false
		a.mu.Unlock()
		slog.Info("broker seat gone", "grant", b.named().Name, "reason", resp.AgentGone.Reason)
	}
}

// errBrokerClosed is what call returns once the connection is gone.
var errBrokerClosed = errors.New("claudia: broker client closed")

// startViaBrokerContext starts (or reclaims) a seat through the daemon.
// errNoBroker when nothing listens; errBrokerNotAvailable when a bare
// protocol server answered. Any other error is the daemon's verdict and is
// not a reason to fall back: a seat the daemon refused to start would not
// start locally either, and a silent local start would fork the fleet.
func startViaBrokerContext(ctx context.Context, cfg Config) (*Agent, error) {
	client, err := dialBroker()
	if err != nil {
		return nil, err
	}
	backend := &brokerAgentBackend{client: client, cfg: cfg, hint: grantHintFrom(ctx)}
	// The daemon owns the terminal log, the goal loop and the MCP
	// materialisation; this process must not open a second log, run a
	// second loop, or write provider config.
	// The Goal stays on the handle so Goal() / GoalActive() report the
	// seat's contract; noteGoalEvent does not run the continuation loop
	// on a broker-held handle (the daemon's Agent does), and CloseGoal
	// forwards to the daemon.
	local := cfg
	local.TermLogPath = "-"
	local.GoalCompleteCheck = nil
	a, err := startWithBackendContext(ctx, local, backend)
	if err != nil {
		client.Close()
		return nil, err
	}
	return a, nil
}

// brokerUsage reads the daemon's plan-usage snapshot (🎯T2.9).
func brokerUsage(ctx context.Context, refresh bool) ([]PlanUsage, time.Time, error) {
	client, err := dialBroker()
	if err != nil {
		return nil, time.Time{}, err
	}
	defer client.Close()
	resp, err := client.call(ctx, &broker.Request{Type: broker.TypeUsage, Usage: &broker.UsageRequest{Refresh: refresh}})
	if err != nil {
		return nil, time.Time{}, err
	}
	if resp.Usage == nil {
		return nil, time.Time{}, fmt.Errorf("broker: usage answered with %s", resp.Type)
	}
	var out []PlanUsage
	if len(resp.Usage.Backends) > 0 {
		if err := json.Unmarshal(resp.Usage.Backends, &out); err != nil {
			return nil, time.Time{}, fmt.Errorf("broker usage: %w", err)
		}
	}
	if resp.Usage.FetchedAt.IsZero() && resp.Usage.Error != "" {
		return nil, time.Time{}, fmt.Errorf("broker usage: %s", resp.Usage.Error)
	}
	return out, resp.Usage.FetchedAt, nil
}
