// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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
	// pool, when set, asks for a warm seat from the daemon's pool
	// (Acquire) instead of a named seat.
	pool *broker.PoolGrant
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

// grantStartTimeout bounds a grant round trip. It is derived from
// readyOverallTimeout rather than quoting it, so the two cannot drift
// apart again: when 🎯T108 raised the readiness bound from its old 30s,
// a grant bound written as a literal would have fired while the daemon
// was still inside a readiness wait it was entitled to.
const grantStartTimeout = readyOverallTimeout + time.Minute

// brokerOpTimeout bounds one ordinary op (send, interrupt, set_model, …).
// Send on the daemon blocks on readiness, so it must outlast a whole
// readiness wait; a client that gives up first reports a failure for a
// send the daemon then goes on to deliver.
const brokerOpTimeout = readyOverallTimeout + 30*time.Second

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
	def, err := EncodeGrantDefinition(configToGrantDef(name, cfg, b.hint.def))
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
			Name: name, Def: def, Adopt: b.hint.adopt, Fallback: b.hint.fallback, Pool: b.hint.pool,
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
		// steer forwards mode=steer (🎯T72.3); the daemon runs the
		// provider's mechanism and the outcome comes back on the sent
		// response. A daemon that predates send.mode answers the bare
		// name, which reads as a submit that was not steered.
		steer: func(_ *Agent, text string) (DeliveryOutcome, error) {
			resp, err := b.opCall(&broker.Request{Type: broker.TypeSend,
				Send: &broker.SendRequest{Name: b.named().Name, Text: text, Mode: broker.SendModeSteer}})
			if err != nil {
				return DeliveryOutcome{}, err
			}
			if resp.Sent == nil {
				return DeliveryOutcome{}, fmt.Errorf("broker: send answered with %s", resp.Type)
			}
			return DeliveryOutcome{
				Mode:             DeliveryMode(resp.Sent.Mode),
				PhaseBefore:      TurnPhase(resp.Sent.PhaseBefore),
				Mechanism:        resp.Sent.Mechanism,
				SupersededTurnID: resp.Sent.SupersededTurnID,
			}, nil
		},
		// turnCaps is the daemon's answer for the seat it actually runs;
		// a daemon that predates turn_caps falls back to the provider
		// contract, which is what a direct handle would start from.
		turnCaps: func(*Agent) TurnCaps {
			resp, err := b.opCall(&broker.Request{Type: broker.TypeAgentInfo, AgentInfo: b.named()})
			if err != nil || resp.AgentInfo == nil || resp.AgentInfo.TurnCaps == nil {
				return ProviderTurnCaps(b.cfg.Provider)
			}
			w := resp.AgentInfo.TurnCaps
			return TurnCaps{
				CanInterrupt:       w.CanInterrupt,
				CanSteer:           w.CanSteer,
				SteerPolicy:        SteerPolicy(w.SteerPolicy),
				BusyOnSecondSubmit: w.BusyOnSecondSubmit,
			}
		},
		migrate: b.migrate,
		rewind:  b.rewind,
		release: b.release,
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
	b.repoint(a, seatWhere{provider: Provider(m.Provider), sessionID: m.SessionID, model: m.Model, windowID: m.WindowID,
		jsonlPath: m.JSONLPath, termLogPath: m.TermLogPath, attach: m.AttachCommand})
	return nil
}

// rewind asks the daemon to roll the seat back and relaunch it, then
// re-points this handle at the relaunched process (🎯T75.8).
func (b *brokerAgentBackend) rewind(a *Agent, n int) (*RewindResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), grantStartTimeout)
	defer cancel()
	resp, err := b.client.call(ctx, &broker.Request{Type: broker.TypeRewind, Rewind: &broker.RewindRequest{Name: b.named().Name, Turns: n}})
	if err != nil {
		return nil, err
	}
	w := resp.Rewound
	if w == nil {
		return nil, fmt.Errorf("broker: rewind answered with %s", resp.Type)
	}
	b.repoint(a, seatWhere{provider: Provider(w.Provider), sessionID: w.SessionID, model: w.Model, windowID: w.WindowID,
		jsonlPath: w.JSONLPath, termLogPath: w.TermLogPath, attach: w.AttachCommand})
	return &RewindResult{
		SessionID: w.SessionID, JSONLPath: w.JSONLPath, TurnsRemoved: w.TurnsRemoved,
		LinesRemoved: w.LinesRemoved, BytesRemoved: w.BytesRemoved, BackupPath: w.BackupPath,
	}, nil
}

// release hands an acquired seat back to the daemon's pool, mapping
// Agent.Release's dispositions onto the wire: return and keep_alive_for
// are reuse, drop is stop. A named seat has no pool to go back to.
func (b *brokerAgentBackend) release(a *Agent, disposition string) error {
	if b.hint.pool == nil {
		return fmt.Errorf("pool: seat %s was not acquired from a pool; use Stop", b.named().Name)
	}
	req := &broker.ReleaseRequest{Name: b.named().Name}
	switch {
	case disposition == "return":
		req.Disposition = broker.DispositionReuse
	case disposition == "drop":
		req.Disposition = broker.DispositionStop
	case strings.HasPrefix(disposition, "keep_alive_for:"):
		secs, err := strconv.ParseInt(strings.TrimPrefix(disposition, "keep_alive_for:"), 10, 64)
		if err != nil || secs <= 0 {
			return fmt.Errorf("pool: invalid keep_alive_for seconds %q", strings.TrimPrefix(disposition, "keep_alive_for:"))
		}
		req.Disposition, req.KeepAliveSeconds = broker.DispositionReuse, secs
	default:
		return fmt.Errorf("pool: unknown disposition %q (want: return, drop, keep_alive_for:<secs>)", disposition)
	}
	_, err := b.opCall(&broker.Request{Type: broker.TypeRelease, Release: req})
	b.client.Close()
	a.markDead()
	return err
}

// acquireViaBroker asks the daemon for a warm seat from its pool (🎯T64).
// errNoBroker / errBrokerNotAvailable mean Acquire takes its own pool.
func acquireViaBroker(ctx context.Context, cfg Config) (*Agent, error) {
	hint := grantHint{pool: &broker.PoolGrant{Policy: cfg.PoolPolicy, Cap: cfg.PoolCap}}
	return startViaBrokerContext(withGrantHint(ctx, hint), cfg)
}

// seatWhere is where a daemon says a seat now lives.
type seatWhere struct {
	provider                                                   Provider
	sessionID, model, windowID, jsonlPath, termLogPath, attach string
}

// repoint makes this handle describe the seat's new process after the
// daemon swapped it (Migrate, Rewind). Subscriptions are the handle's and
// stay.
func (b *brokerAgentBackend) repoint(a *Agent, w seatWhere) {
	b.mu.Lock()
	b.attach = w.attach
	b.mu.Unlock()
	a.mu.Lock()
	a.backendGen.Add(1)
	a.provider = w.provider
	a.sessionID = w.sessionID
	a.jsonlPath = w.jsonlPath
	a.tmuxWindowID = w.windowID
	if w.model != "" {
		a.model = w.model
	}
	a.mu.Unlock()
	a.termMu.Lock()
	if w.termLogPath != "" {
		a.termLogPath = w.termLogPath
		a.termLogLive = true
	} else {
		a.termLogLive = false
	}
	a.termMu.Unlock()
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
					a.markDead()
					return
				}
			}
		}
	}
}

func (b *brokerAgentBackend) deliver(a *Agent, resp *broker.Response) {
	switch resp.Type {
	case broker.TypeAgentEvent:
		ev, err := DecodeEventWire(resp.AgentEvent.Event)
		if err != nil {
			slog.Warn("broker agent event undecodable", "grant", b.named().Name, "err", err)
			return
		}
		a.publishEvent(ev)
	case broker.TypeAgentTerm:
		a.pushTermOutput(resp.AgentTerm.Data)
	case broker.TypeGoalCheck:
		// The consumer's check is user code and may block; the queue
		// must keep delivering the seat's events meanwhile.
		go b.answerGoalCheck(a, resp.GoalCheck)
	case broker.TypeAgentGone:
		b.mu.Lock()
		b.gone = true
		b.mu.Unlock()
		a.markDead()
		slog.Info("broker seat gone", "grant", b.named().Name, "reason", resp.AgentGone.Reason)
	}
}

// answerGoalCheck runs this handle's GoalCompleteCheck for the daemon's
// Goal loop and sends the verdict (🎯T75.9).
func (b *brokerAgentBackend) answerGoalCheck(a *Agent, m *broker.GoalCheckMessage) {
	a.mu.Lock()
	check := a.goalCompleteCheck
	a.mu.Unlock()
	complete := check != nil && check(m.Goal, m.TurnText)
	if complete {
		// The daemon closes the seat's Goal on this verdict; GoalActive on
		// the handle reports the same, as it does in direct mode.
		a.closeGoal()
	}
	if _, err := b.opCall(&broker.Request{Type: broker.TypeGoalVerdict, GoalVerdict: &broker.GoalVerdictRequest{
		Name: m.Name, CheckID: m.CheckID, Complete: complete, Answered: check != nil,
	}}); err != nil {
		slog.Warn("broker goal_verdict failed", "grant", m.Name, "err", err)
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
	// forwards to the daemon. GoalCompleteCheck stays too: the daemon's
	// loop asks this handle for its verdict over goal_check (🎯T75.9).
	local := cfg
	local.TermLogPath = "-"
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
