// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/marcelocantos/claudia/internal/broker"
)

// The claudia daemon (🎯T2: metaharness). It is the process that runs agents
// and keeps them running: consumers name seats over the socket, the daemon
// starts the provider processes as their parent, streams their Events back,
// and keeps the seat when the consumer's connection goes away so the
// consumer can reclaim it after its own restart (🎯T2.10, 🎯T2.11). It is
// also the one plan-usage evaluator on the host (🎯T2.9), and after a host
// reboot it brings back the seats it held and tells them so.
//
// The daemon's registry is a claudia Registry in direct mode: the same
// lifecycle code every consumer already relies on (reservations, resume
// fail-closed, adopt of tmux windows and connect-mode serves), persisted
// under the daemon's state directory. A seat is a registered definition
// plus the daemon's ownership record.

// BrokerDaemonOptions configures a daemon. Zero values are the defaults a
// `claudia broker serve` gets.
type BrokerDaemonOptions struct {
	// SocketPath overrides the socket (default: broker.SocketPath, which
	// honours CLAUDIA_BROKER_SOCKET).
	SocketPath string
	// StateDir holds grants.json (default: the claudia state directory).
	StateDir string
	// UsageTTL is how long a usage snapshot is current. Zero uses
	// DefaultPlanCacheTTL.
	UsageTTL time.Duration
	// UsageFetch replaces the vendor usage query (tests).
	UsageFetch func(context.Context) ([]PlanUsage, error)
	// DisableResume skips bringing back the seats held before the daemon
	// last stopped.
	DisableResume bool
	// RestartNudge is the message sent to a seat the daemon had to relaunch
	// from its transcript on boot. Empty uses DefaultRestartNudge;
	// NoRestartNudge ("-") sends nothing.
	RestartNudge string
	// ResumeConcurrency bounds how many seats resume at once. Zero means 2.
	ResumeConcurrency int
	// Logger receives daemon logs. Nil uses slog.Default.
	Logger *slog.Logger

	clock broker.Clock
	// resumeGate, when set, holds the boot resume until closed (tests
	// subscribe to the tail first).
	resumeGate chan struct{}
	// DisableMCPHost skips the loopback MCP listener (tests that do not
	// want a port). Production serve always hosts MCP (🎯T2.16).
	DisableMCPHost bool
	// MCPListenAddr overrides the MCP loopback bind (default 127.0.0.1:0).
	MCPListenAddr string
	// MCPConsumerOwnedPrefixes names MCP servers (by name prefix) the host
	// leaves on the consumer's URL. Nil keeps jevonsmcp*; empty hosts all.
	MCPConsumerOwnedPrefixes []string
	// IntelInterval is how often the daemon refreshes model intel.
	// Zero uses DefaultModelIntelInterval (24h).
	IntelInterval time.Duration
	// DisableIntel skips the daily ingest (tests).
	DisableIntel bool
	// IntelRefresh replaces RefreshModelIntel (tests).
	IntelRefresh func(context.Context) error
}

// Daemon internals.
const (
	// brokerUnownedRingCap is how many events an unowned seat retains for
	// the next reclaim. A jevonsd-length bounce can stream far more than
	// the old 256-event bound; overflowing that marked Lagged and dropped
	// the live turn (🎯T70). Beyond this cap the oldest are dropped and
	// the reclaim is marked lagged, never silently truncated.
	brokerUnownedRingCap = 100_000
	// brokerPumpSize bounds the per-owner outbound queue. A consumer that
	// stops reading is detached, not allowed to stall the seat.
	brokerPumpSize = 1024
	// grantsFile is the daemon's registry under StateDir.
	grantsFile = "grants.json"
	// connOwnedKey is the per-connection list of grant names it owns.
	connOwnedKey = "owned"
)

// BrokerDaemon is a running daemon.
type BrokerDaemon struct {
	opts  BrokerDaemonOptions
	log   *slog.Logger
	clock broker.Clock
	reg   *Registry
	srv   *broker.Server
	usage *PlanUsageMonitor
	path  string
	// stateDir is the resolved state directory. opts.StateDir is empty on a
	// default serve and must not be read after construction.
	stateDir string
	mcp      *MCPHost
	// seatSub is the daemon's subscription to its Registry's seat events.
	seatSub int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// resumeDone closes when the boot resume has finished (or was disabled).
	resumeDone chan struct{}

	mu     sync.Mutex
	grants map[string]*brokerGrant
	tasks  map[string]*brokerDaemonTask
	checks map[string]pendingGoalCheck
}

// brokerGrant is the daemon's ownership record for one seat.
type brokerGrant struct {
	name  string
	owner *broker.ClientConn
	pump  chan []byte
	proc  *Agent
	sub   int64
	ring  [][]byte
	lag   bool
	termQ chan []byte
}

type brokerDaemonTask struct {
	task   *Task
	cancel context.CancelFunc
	owner  *broker.ClientConn
}

// NewBrokerDaemon binds the socket and starts serving. Close stops it.
func NewBrokerDaemon(opts BrokerDaemonOptions) (*BrokerDaemon, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	clock := opts.clock
	if clock == nil {
		clock = broker.SystemClock{}
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		dir, err := broker.StateDir()
		if err != nil {
			return nil, err
		}
		stateDir = dir
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("broker daemon: state dir: %w", err)
	}
	path := opts.SocketPath
	if path == "" {
		p, err := broker.SocketPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	reg, err := NewRegistry(filepath.Join(stateDir, grantsFile))
	if err != nil {
		return nil, fmt.Errorf("broker daemon: grants: %w", err)
	}
	reg.direct = true

	ln, err := broker.Listen(path)
	if err != nil {
		return nil, err
	}
	d := &BrokerDaemon{
		opts:     opts,
		log:      log,
		clock:    clock,
		reg:      reg,
		path:     path,
		stateDir: stateDir,
		grants:   map[string]*brokerGrant{},
		tasks:    map[string]*brokerDaemonTask{},
		checks:   map[string]pendingGoalCheck{},
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.resumeDone = make(chan struct{})
	d.usage = NewPlanUsageMonitor(&PlanUsageMonitorArgs{
		TTL: opts.UsageTTL, Fetch: opts.UsageFetch, Clock: clock,
		OnUpdate: func(fetched []PlanUsage, err error) {
			if err != nil {
				d.log.Warn("plan usage fetch failed", "err", err)
				return
			}
			for _, u := range fetched {
				d.emit(broker.EventMessage{Kind: broker.EventUsageUpdate, Detail: string(u.Provider)})
			}
		},
	})
	srv, err := broker.Serve(&broker.ServeArgs{Listener: ln, Clock: clock, Handler: d})
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	d.srv = srv
	if !opts.DisableMCPHost {
		var seeds []string
		if home, err := os.UserHomeDir(); err == nil {
			// Jevons owned MCP connections before the daemon did; its tokens
			// seed an empty store so Atlassian and friends do not re-prompt.
			seeds = append(seeds, filepath.Join(home, ".jevons"))
		}
		h, err := NewMCPHost(&MCPHostArgs{
			StateDir:      stateDir,
			ListenAddr:    opts.MCPListenAddr,
			ConsumerOwned: consumerOwnedByPrefix(opts.MCPConsumerOwnedPrefixes),
			SeedStateDirs: seeds,
			Logger:        func(msg string, args ...any) { log.Warn(msg, args...) },
		})
		if err != nil {
			_ = srv.Close()
			return nil, fmt.Errorf("broker daemon: mcp host: %w", err)
		}
		d.mcp = h
		reg.SetMCPHost(h)
		log.Info("claudia mcp host listening", "addr", h.Addr())
	}
	reg.clock = clock
	d.seatSub = reg.SubscribeSeatEvents(d.onSeatEvent)
	d.wg.Add(1)
	go func() { defer d.wg.Done(); d.usage.Run(d.ctx) }()
	if !opts.DisableIntel {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			RunModelIntelRefresher(d.ctx, &ModelIntelRefresherArgs{
				Dir:      filepath.Join(stateDir, ModelIntelDirName),
				Interval: opts.IntelInterval,
				Clock:    clock,
				Refresh:  opts.IntelRefresh,
				OnError:  func(err error) { log.Debug("model intel refresh skipped", "err", err) },
			})
		}()
	}
	if !opts.DisableResume {
		d.wg.Add(1)
		go func() { defer d.wg.Done(); defer close(d.resumeDone); d.resumeSeats() }()
	} else {
		close(d.resumeDone)
	}
	log.Info("claudia broker serving", "socket", path, "state", stateDir)
	return d, nil
}

// SocketPath is where the daemon listens.
func (d *BrokerDaemon) SocketPath() string { return d.path }

// Run blocks until ctx is done, then closes.
func (d *BrokerDaemon) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
	case <-d.ctx.Done():
	}
	return d.Close()
}

// Close stops serving. Seats are left running: tmux windows and
// connect-mode serves survive for the next daemon (resume adopts them);
// stdio children die with this process and are relaunched from their
// transcripts on the next boot (resumeSeats).
func (d *BrokerDaemon) Close() error {
	d.cancel()
	d.reg.UnsubscribeSeatEvents(d.seatSub)
	err := d.srv.Close()
	d.mu.Lock()
	tasks := d.tasks
	d.tasks = map[string]*brokerDaemonTask{}
	d.mu.Unlock()
	for _, t := range tasks {
		t.cancel()
	}
	d.wg.Wait()
	if d.mcp != nil {
		if cerr := d.mcp.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// consumerOwnedByPrefix is the daemon's MCPHostArgs.ConsumerOwned: a server
// whose name starts with one of prefixes (case-insensitive) stays on the
// consumer's URL.
func consumerOwnedByPrefix(prefixes []string) func(MCPServer) bool {
	if prefixes == nil {
		prefixes = defaultMCPConsumerOwnedPrefixes
	}
	return func(s MCPServer) bool {
		name := strings.ToLower(strings.TrimSpace(s.Name))
		for _, p := range prefixes {
			if p != "" && strings.HasPrefix(name, strings.ToLower(p)) {
				return true
			}
		}
		return false
	}
}

// defaultMCPConsumerOwnedPrefixes keeps Jevons's own MCP server (served by
// jevonsd for each consumer) on the consumer's URL.
var defaultMCPConsumerOwnedPrefixes = []string{"jevonsmcp"}

func (d *BrokerDaemon) emit(ev broker.EventMessage) {
	if ev.At.IsZero() {
		ev.At = d.clock.Now()
	}
	d.srv.Emit(ev)
}

// HandleRequest implements broker.Handler.
func (d *BrokerDaemon) HandleRequest(c *broker.ClientConn, req *broker.Request) bool {
	switch req.Type {
	case broker.TypeGrant:
		d.handleGrant(c, req)
	case broker.TypeRelease:
		if req.Release.Name == "" {
			return false
		}
		d.handleRelease(c, req)
	case broker.TypeSend, broker.TypeInterrupt, broker.TypeSetModel, broker.TypeMigrate,
		broker.TypeAgentInfo, broker.TypeTermSubscribe, broker.TypeResize, broker.TypeCloseGoal, broker.TypeRewind:
		d.handleAgentOp(c, req)
	case broker.TypeGoalVerdict:
		d.handleGoalVerdict(c, req)
	case broker.TypeTaskRun:
		d.handleTaskRun(c, req)
	case broker.TypeTaskCancel:
		d.handleTaskCancel(c, req)
	case broker.TypeUsage:
		snap := d.usage.Read(d.ctx, req.Usage.Refresh)
		raw, _ := json.Marshal(snap.Backends)
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeUsageResult,
			Usage: &broker.UsageResponse{FetchedAt: snap.FetchedAt, Backends: raw, Error: snap.Err}})
	case broker.TypeResolve:
		d.handleResolve(c, req)
	case broker.TypeStatus:
		d.handleStatus(c, req)
	case broker.TypeGrants:
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGrantsResult,
			Grants: &broker.GrantsResponse{Grants: d.grantList()}})
	case broker.TypeSpawn:
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeUnsupportedValue, Field: "type", Value: string(req.Type),
			Msg: "the daemon grants seats by name; use grant (Session) or task_run (Task)"})
	default:
		return false
	}
	return true
}

// ConnClosed implements broker.Handler: seats stay running, unowned.
func (d *BrokerDaemon) ConnClosed(c *broker.ClientConn) {
	d.mu.Lock()
	var detached []string
	for name, g := range d.grants {
		if g.owner == c {
			d.detachLocked(g)
			detached = append(detached, name)
		}
	}
	var cancels []context.CancelFunc
	for id, t := range d.tasks {
		if t.owner == c {
			cancels = append(cancels, t.cancel)
			delete(d.tasks, id)
		}
	}
	d.mu.Unlock()
	for _, name := range detached {
		d.log.Info("consumer connection closed; seat kept running", "grant", name)
		d.emit(broker.EventMessage{Kind: broker.EventDetach, Name: name})
	}
	for _, cancel := range cancels {
		cancel()
	}
}

// detachLocked drops ownership and stops the owner's pump. d.mu held.
func (d *BrokerDaemon) detachLocked(g *brokerGrant) {
	g.owner = nil
	if g.pump != nil {
		close(g.pump)
		g.pump = nil
	}
	if g.termQ != nil && g.proc != nil {
		// Closes the channel, which ends the terminal pump goroutine.
		g.proc.UnsubscribeTerminal(g.termQ)
		g.termQ = nil
	}
}

// ownedBy records name on the connection so status can show who holds what.
func ownedBy(c *broker.ClientConn, name string) {
	v, _ := c.Get(connOwnedKey)
	names, _ := v.([]string)
	c.Set(connOwnedKey, append(names, name))
}

func (d *BrokerDaemon) handleGrant(c *broker.ClientConn, req *broker.Request) {
	name := req.Grant.Name
	wire, err := DecodeGrantDefinition(req.Grant.Def)
	if err != nil {
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeMalformed, Field: "def", Msg: err.Error()})
		return
	}
	def := wire.AgentDef
	def.Name = name
	if wire.RequireResume {
		def.Materialized = true
	}
	if def.SessionID == "" {
		def.SessionID = uuid.New().String()
	}
	if def.WorkDir == "" {
		def.WorkDir = "."
	}
	// A seat is marked live (AutoStart) while the daemon holds it, so a
	// daemon that comes back after a reboot knows which seats to bring back.
	def.AutoStart = true

	d.mu.Lock()
	g := d.grants[name]
	if g != nil && g.owner != nil && g.owner != c {
		d.mu.Unlock()
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeGrantHeld, Field: "name", Value: name,
			Msg: fmt.Sprintf("grant %s is owned by another connection", name)})
		return
	}
	d.mu.Unlock()

	// Merge the daemon's runtime knowledge onto the consumer's definition:
	// the consumer does not know the connect-mode endpoint or that the
	// seat has materialized. A different session id is a deliberate
	// remint by the consumer and wins.
	if existing := d.reg.Def(name); existing != nil {
		if def.SessionID == existing.SessionID {
			def.ConnectURL, def.ConnectPID = existing.ConnectURL, existing.ConnectPID
			def.Materialized = def.Materialized || existing.Materialized
			def.GrokConnect = def.GrokConnect || existing.GrokConnect
		}
	}
	if err := d.reg.Register(def); err != nil && !errors.Is(err, ErrLifecycleInProgress) {
		_ = c.Fail(req.ID, err)
		return
	}

	ctx, cancel := context.WithTimeout(d.ctx, grantStartTimeout)
	defer cancel()
	var proc *Agent
	switch {
	case req.Grant.Adopt && req.Grant.Fallback:
		proc, err = d.reg.AdoptOrLaunchContext(ctx, name)
	case req.Grant.Adopt:
		proc, err = d.reg.Adopt(name)
	default:
		proc, err = d.reg.LaunchContext(ctx, name)
	}
	if err != nil {
		_ = c.Fail(req.ID, err)
		return
	}

	d.mu.Lock()
	g = d.grants[name]
	if g == nil {
		g = &brokerGrant{name: name}
		d.grants[name] = g
	}
	reclaimed := g.proc == proc && g.proc != nil
	if g.proc != proc {
		d.bindSeatLocked(g, proc)
	}
	if g.owner != nil && g.owner != c {
		d.detachLocked(g)
	}
	ring, lagged := g.ring, g.lag
	g.ring, g.lag = nil, false
	g.owner = c
	pump := make(chan []byte, brokerPumpSize)
	g.pump = pump
	d.mu.Unlock()
	ownedBy(c, name)
	d.wg.Add(1)
	go func() { defer d.wg.Done(); d.runPump(g, c, pump) }()

	// Replay what the seat said while unowned, then confirm the grant so
	// the consumer sees history before live traffic.
	for _, raw := range ring {
		_ = c.Reply(&broker.Response{Type: broker.TypeAgentEvent, AgentEvent: &broker.AgentEventMessage{Name: name, Event: raw}})
	}
	resp := &broker.GrantResponse{
		Name:          name,
		SessionID:     proc.SessionID(),
		Provider:      broker.Provider(procProvider(proc)),
		Model:         proc.Model(),
		WindowID:      proc.WindowID(),
		JSONLPath:     proc.JSONLPath(),
		TermLogPath:   proc.TermLogPath(),
		AttachCommand: proc.AttachCommand(),
		ConnectURL:    proc.ConnectURL(),
		ConnectPID:    proc.PID(),
		Reclaimed:     reclaimed,
		Replayed:      len(ring),
		Lagged:        lagged,
		TurnCaps:      turnCapsWire(proc),
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGranted, Granted: resp})
	d.emit(broker.EventMessage{Kind: broker.EventGrant, Name: name, SessionID: resp.SessionID,
		Detail: map[bool]string{true: "reclaimed", false: "started"}[reclaimed]})
}

func procProvider(a *Agent) Provider {
	if p := a.Provider(); p != "" {
		return p
	}
	return ProviderClaude
}

// bindSeatLocked points grant g at proc: the seat's events flow to g's
// owner, and its Goal loop asks g's owner for a completeness verdict
// (🎯T75.9). d.mu held.
func (d *BrokerDaemon) bindSeatLocked(g *brokerGrant, proc *Agent) {
	if g.proc != nil && g.sub != 0 {
		g.proc.UnsubscribeEvents(g.sub)
	}
	g.proc = proc
	g.sub = proc.SubscribeEvents(d.forwarder(g.name))
	proc.SetGoalCompleteCheck(d.askOwnerGoalCheck(g.name))
}

// goalCheckTimeout bounds how long a seat's Goal loop waits for its owner's
// verdict before continuing as if there were none.
const goalCheckTimeout = 30 * time.Second

// pendingGoalCheck is one goal_check awaiting its owner's verdict.
type pendingGoalCheck struct {
	owner   *broker.ClientConn
	verdict chan bool
}

// askOwnerGoalCheck is the GoalCompleteCheck a daemon-held seat runs: it
// asks whichever connection owns the seat at the time. No owner, an owner
// without a check, a lost connection or no answer in time all mean "not
// complete", which leaves the seat on ParseGoalStatus as before.
func (d *BrokerDaemon) askOwnerGoalCheck(name string) func(goal, turnText string) bool {
	return func(goal, turnText string) bool {
		d.mu.Lock()
		g := d.grants[name]
		if g == nil || g.owner == nil {
			d.mu.Unlock()
			return false
		}
		owner := g.owner
		id := newRunID()
		verdict := make(chan bool, 1)
		d.checks[id] = pendingGoalCheck{owner: owner, verdict: verdict}
		d.mu.Unlock()
		defer func() {
			d.mu.Lock()
			delete(d.checks, id)
			d.mu.Unlock()
		}()
		if err := owner.Reply(&broker.Response{Type: broker.TypeGoalCheck, GoalCheck: &broker.GoalCheckMessage{
			Name: name, CheckID: id, Goal: goal, TurnText: turnText,
		}}); err != nil {
			return false
		}
		select {
		case complete := <-verdict:
			return complete
		case <-d.clock.After(goalCheckTimeout):
			d.log.Warn("goal check unanswered; continuing", "grant", name)
			return false
		case <-d.ctx.Done():
			return false
		}
	}
}

// handleGoalVerdict delivers an owner's answer to the check that asked.
// A verdict for a check that already gave up is acknowledged and dropped.
func (d *BrokerDaemon) handleGoalVerdict(c *broker.ClientConn, req *broker.Request) {
	v := req.GoalVerdict
	d.mu.Lock()
	p, ok := d.checks[v.CheckID]
	d.mu.Unlock()
	if ok && p.owner != c {
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeNotOwner, Field: "check_id", Value: v.CheckID,
			Msg: "goal check was sent to another connection"})
		return
	}
	if ok {
		select {
		case p.verdict <- v.Answered && v.Complete:
		default:
		}
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGoalVerdictNoted, GoalVerdictNoted: &broker.NamedResponse{Name: v.Name}})
}

// forwarder is the daemon-side event subscriber for one seat.
func (d *BrokerDaemon) forwarder(name string) EventFunc {
	return func(ev Event) {
		if ev.Type == "system" && ev.ProgressType == ProgressStuck {
			// Synthesized by the daemon-side Agent from the event before
			// it, which also invalidated the daemon's usage monitor; the
			// consumer's Agent synthesizes its own from the same event,
			// so forwarding this one would double it.
			return
		}
		raw, err := EncodeEventWire(ev)
		if err != nil {
			d.log.Warn("event not encodable", "grant", name, "err", err)
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		g := d.grants[name]
		if g == nil {
			return
		}
		if g.pump != nil {
			select {
			case g.pump <- raw:
				return
			default:
				d.log.Warn("consumer not reading; detaching seat", "grant", name)
				d.detachLocked(g)
			}
		}
		g.retainUnowned(raw)
	}
}

// retainUnowned appends one event to the reclaim ring. d.mu / caller
// must hold the grant lock. A live bounce stream must not mark Lagged
// at the old 256 bound (🎯T70).
func (g *brokerGrant) retainUnowned(raw []byte) {
	if g == nil {
		return
	}
	g.ring = append(g.ring, raw)
	if len(g.ring) > brokerUnownedRingCap {
		g.ring = g.ring[len(g.ring)-brokerUnownedRingCap:]
		g.lag = true
	}
}

// runPump writes queued events to the owner until detached or the write
// fails.
func (d *BrokerDaemon) runPump(g *brokerGrant, c *broker.ClientConn, pump chan []byte) {
	for raw := range pump {
		if err := c.Reply(&broker.Response{Type: broker.TypeAgentEvent, AgentEvent: &broker.AgentEventMessage{Name: g.name, Event: raw}}); err != nil {
			d.mu.Lock()
			if g.owner == c {
				d.detachLocked(g)
			}
			g.retainUnowned(raw)
			d.mu.Unlock()
			return
		}
	}
}

func (d *BrokerDaemon) handleRelease(c *broker.ClientConn, req *broker.Request) {
	name := req.Release.Name
	d.mu.Lock()
	g := d.grants[name]
	if g == nil && d.reg.Def(name) == nil {
		d.mu.Unlock()
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeUnknownGrant, Field: "name", Value: name,
			Msg: fmt.Sprintf("grant %s is not one the daemon holds", name)})
		return
	}
	if g != nil && g.owner != nil && g.owner != c {
		d.mu.Unlock()
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeNotOwner, Field: "name", Value: name,
			Msg: fmt.Sprintf("grant %s belongs to another connection", name)})
		return
	}
	switch req.Release.Disposition {
	case broker.DispositionDetach:
		if g != nil {
			d.detachLocked(g)
		}
		d.mu.Unlock()
		d.emit(broker.EventMessage{Kind: broker.EventDetach, Name: name})
	case broker.DispositionStop:
		if g != nil {
			d.detachLocked(g)
			if g.proc != nil && g.sub != 0 {
				g.proc.UnsubscribeEvents(g.sub)
			}
			delete(d.grants, name)
		}
		d.mu.Unlock()
		if err := d.reg.Remove(name); err != nil {
			d.log.Warn("release stop", "grant", name, "err", err)
		}
		d.emit(broker.EventMessage{Kind: broker.EventRelease, Name: name})
	default:
		d.mu.Unlock()
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeUnsupportedValue, Field: "disposition",
			Value: string(req.Release.Disposition), Msg: "a granted seat is released with stop or detach"})
		return
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeReleased,
		Released: &broker.ReleaseResponse{Name: name, Disposition: req.Release.Disposition}})
}

// deliverSend dispatches one send by its mode (🎯T72.3). submit is
// Agent.Send (SendMode with DeliverySubmit is that call plus the outcome);
// steer and interrupt are the 🎯T72.2 verbs, which fold an idle seat down
// to a plain submit and say so in the mechanism; queue is ack-only, because
// the design keeps the queue host-side and the daemon holds none. The
// returned response has every field but Name.
func deliverSend(proc *Agent, req *broker.SendRequest) (*broker.SentResponse, error) {
	var out DeliveryOutcome
	var err error
	switch req.Mode {
	case broker.SendModeSubmit:
		out, err = proc.SendMode(req.Text, DeliverySubmit)
	case broker.SendModeSteer:
		out, err = proc.SendMode(req.Text, DeliverySteer)
	case broker.SendModeInterrupt:
		out, err = proc.SendMode(req.Text, DeliveryInterrupt)
	case broker.SendModeQueue:
		out = DeliveryOutcome{Mode: DeliveryQueue, PhaseBefore: proc.TurnPhase(), Mechanism: MechanismClientQueue}
	default:
		return nil, &broker.ProtocolError{Code: broker.CodeUnsupportedValue, Field: "mode", Value: string(req.Mode),
			Msg: fmt.Sprintf("send mode %q is not one the daemon dispatches", req.Mode)}
	}
	if err != nil {
		return nil, err
	}
	return &broker.SentResponse{
		Mode:             broker.SendMode(out.Mode),
		Mechanism:        out.Mechanism,
		PhaseBefore:      string(out.PhaseBefore),
		SupersededTurnID: out.SupersededTurnID,
	}, nil
}

// turnCapsWire is the seat's TurnCaps as the wire spells them.
func turnCapsWire(proc *Agent) *broker.TurnCaps {
	caps := proc.TurnCaps()
	return &broker.TurnCaps{
		CanInterrupt:       caps.CanInterrupt,
		CanSteer:           caps.CanSteer,
		SteerPolicy:        string(caps.SteerPolicy),
		BusyOnSecondSubmit: caps.BusyOnSecondSubmit,
	}
}

// seatFor resolves a request's grant and, when needOwner, checks c owns it.
func (d *BrokerDaemon) seatFor(c *broker.ClientConn, id, name string, needOwner bool) (*brokerGrant, *Agent, bool) {
	d.mu.Lock()
	g := d.grants[name]
	d.mu.Unlock()
	if g == nil || g.proc == nil {
		_ = c.Fail(id, &broker.ProtocolError{Code: broker.CodeUnknownGrant, Field: "name", Value: name,
			Msg: fmt.Sprintf("grant %s is not one the daemon holds", name)})
		return nil, nil, false
	}
	if needOwner && g.owner != c {
		_ = c.Fail(id, &broker.ProtocolError{Code: broker.CodeNotOwner, Field: "name", Value: name,
			Msg: fmt.Sprintf("grant %s is not owned by this connection", name)})
		return nil, nil, false
	}
	return g, g.proc, true
}

func (d *BrokerDaemon) handleAgentOp(c *broker.ClientConn, req *broker.Request) {
	var name string
	switch req.Type {
	case broker.TypeSend:
		name = req.Send.Name
	case broker.TypeInterrupt:
		name = req.Interrupt.Name
	case broker.TypeSetModel:
		name = req.SetModel.Name
	case broker.TypeMigrate:
		name = req.Migrate.Name
	case broker.TypeAgentInfo:
		name = req.AgentInfo.Name
	case broker.TypeTermSubscribe:
		name = req.TermSubscribe.Name
	case broker.TypeResize:
		name = req.Resize.Name
	case broker.TypeCloseGoal:
		name = req.CloseGoal.Name
	case broker.TypeRewind:
		name = req.Rewind.Name
	}
	g, proc, ok := d.seatFor(c, req.ID, name, req.Type != broker.TypeAgentInfo)
	if !ok {
		return
	}
	named := &broker.NamedResponse{Name: name}
	switch req.Type {
	case broker.TypeSend:
		sent, err := deliverSend(proc, req.Send)
		if err != nil {
			_ = c.Fail(req.ID, err)
			return
		}
		sent.Name = name
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeSent, Sent: sent})
	case broker.TypeInterrupt:
		if err := proc.Interrupt(); err != nil {
			_ = c.Fail(req.ID, err)
			return
		}
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeInterrupted, Interrupted: named})
	case broker.TypeSetModel:
		if err := proc.SetModel(req.SetModel.Model); err != nil {
			_ = c.Fail(req.ID, err)
			return
		}
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeModelSet, ModelSet: named})
	case broker.TypeMigrate:
		if err := proc.Migrate(&MigrateArgs{Provider: Provider(req.Migrate.Provider), Model: req.Migrate.Model,
			Reason: req.Migrate.Reason, Force: req.Migrate.Force}); err != nil {
			_ = c.Fail(req.ID, err)
			return
		}
		// The Registry recorded the destination (🎯T75.3), so a boot
		// resume brings back the seat the consumer is actually on.
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeMigrated, Migrated: &broker.MigrateResponse{
			Name: name, SessionID: proc.SessionID(), Provider: broker.Provider(procProvider(proc)), Model: proc.Model(),
			WindowID: proc.WindowID(), JSONLPath: proc.JSONLPath(), TermLogPath: proc.TermLogPath(), AttachCommand: proc.AttachCommand(),
		}})
	case broker.TypeAgentInfo:
		usage, _ := json.Marshal(proc.Usage())
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeAgentInfoResult, AgentInfo: &broker.AgentInfoResponse{
			Name: name, SessionID: proc.SessionID(), Provider: broker.Provider(procProvider(proc)), Model: proc.Model(),
			Alive: proc.Alive(), PromptInFlight: proc.PromptInFlight(), Usage: usage,
			WindowID: proc.WindowID(), JSONLPath: proc.JSONLPath(), TermLogPath: proc.TermLogPath(),
			AttachCommand: proc.AttachCommand(), ConnectURL: proc.ConnectURL(), ConnectPID: proc.PID(),
			TurnCaps: turnCapsWire(proc),
		}})
	case broker.TypeTermSubscribe:
		history, ch := proc.SubscribeTerminal()
		d.mu.Lock()
		if g.termQ != nil {
			proc.UnsubscribeTerminal(g.termQ)
		}
		g.termQ = ch
		d.mu.Unlock()
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for data := range ch {
				d.mu.Lock()
				owner := g.owner
				d.mu.Unlock()
				if owner != c {
					proc.UnsubscribeTerminal(ch)
					return
				}
				if err := c.Reply(&broker.Response{Type: broker.TypeAgentTerm, AgentTerm: &broker.AgentTermMessage{Name: name, Data: data}}); err != nil {
					proc.UnsubscribeTerminal(ch)
					return
				}
			}
		}()
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeTermSubscribed,
			TermSubscribed: &broker.TermSubscribedResponse{Name: name, History: history}})
	case broker.TypeRewind:
		ctx, cancel := context.WithTimeout(d.ctx, grantStartTimeout)
		next, res, err := d.reg.Rewind(ctx, name, req.Rewind.Turns)
		cancel()
		if next != nil && next != proc {
			// The Registry relaunched the seat: the grant follows the new
			// process, so the owner's stream continues from it.
			d.mu.Lock()
			d.bindSeatLocked(g, next)
			d.mu.Unlock()
		}
		if err != nil {
			_ = c.Fail(req.ID, err)
			return
		}
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeRewound, Rewound: &broker.RewindResponse{
			Name: name, SessionID: next.SessionID(), Provider: broker.Provider(procProvider(next)), Model: next.Model(),
			WindowID: next.WindowID(), JSONLPath: next.JSONLPath(), TermLogPath: next.TermLogPath(), AttachCommand: next.AttachCommand(),
			TurnsRemoved: res.TurnsRemoved, LinesRemoved: res.LinesRemoved, BytesRemoved: res.BytesRemoved, BackupPath: res.BackupPath,
		}})
	case broker.TypeCloseGoal:
		proc.CloseGoal()
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeGoalClosed, GoalClosed: named})
	case broker.TypeResize:
		if err := proc.Resize(req.Resize.Cols, req.Resize.Rows); err != nil {
			_ = c.Fail(req.ID, err)
			return
		}
		_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeResized, Resized: named})
	}
}

func (d *BrokerDaemon) handleTaskRun(c *broker.ClientConn, req *broker.Request) {
	cfg, err := DecodeTaskConfigWire(req.TaskRun.Task)
	if err != nil {
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeMalformed, Field: "task", Msg: err.Error()})
		return
	}
	task := daemonNewTask(cfg)
	runID := newRunID()
	if req.TaskRun.RawLog {
		// The line is only valid during the call; the push copies it.
		task.SetRawLog(func(line []byte) {
			_ = c.Reply(&broker.Response{Type: broker.TypeTaskRaw, TaskRaw: &broker.TaskRawMessage{RunID: runID, Line: string(line)}})
		})
	}
	ctx, cancel := context.WithCancel(d.ctx)
	ch, err := task.Run(ctx, req.TaskRun.Prompt)
	if err != nil {
		cancel()
		_ = c.Fail(req.ID, err)
		return
	}
	d.mu.Lock()
	d.tasks[runID] = &brokerDaemonTask{task: task, cancel: cancel, owner: c}
	d.mu.Unlock()
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeTaskStarted, TaskStarted: &broker.TaskStartedResponse{RunID: runID}})
	d.emit(broker.EventMessage{Kind: broker.EventTaskStart, Name: runID, Detail: string(cfg.Provider)})
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		for ev := range ch {
			raw, err := EncodeTaskEventWire(ev)
			if err != nil {
				continue
			}
			if err := c.Reply(&broker.Response{Type: broker.TypeTaskEvent, TaskEvent: &broker.TaskEventMessage{RunID: runID, Event: raw}}); err != nil {
				// Consumer gone: the run is cancelled with the context.
				break
			}
		}
		d.mu.Lock()
		delete(d.tasks, runID)
		d.mu.Unlock()
		_ = c.Reply(&broker.Response{Type: broker.TypeTaskDone, TaskDone: &broker.TaskDoneMessage{RunID: runID}})
		d.emit(broker.EventMessage{Kind: broker.EventTaskDone, Name: runID})
	}()
}

// daemonNewTask builds a direct-mode Task for one run. Hermetic tests point
// it at a fake backend.
var daemonNewTask = func(cfg TaskConfig) *Task {
	t := NewTask(cfg)
	t.direct = true
	return t
}

func (d *BrokerDaemon) handleTaskCancel(c *broker.ClientConn, req *broker.Request) {
	d.mu.Lock()
	t := d.tasks[req.TaskCancel.RunID]
	d.mu.Unlock()
	if t == nil {
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeUnknownRun, Field: "run_id", Value: req.TaskCancel.RunID,
			Msg: "no such task run"})
		return
	}
	if t.owner != c {
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeNotOwner, Field: "run_id", Value: req.TaskCancel.RunID,
			Msg: "task run belongs to another connection"})
		return
	}
	if err := t.task.Cancel(); err != nil {
		_ = c.Fail(req.ID, err)
		return
	}
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeTaskCancelled, TaskCancelled: &broker.TaskCancelledResponse{RunID: req.TaskCancel.RunID}})
}

func (d *BrokerDaemon) handleResolve(c *broker.ClientConn, req *broker.Request) {
	pred, err := DecodePredicatesWire(req.Resolve.Predicates)
	if err != nil {
		_ = c.Fail(req.ID, &broker.ProtocolError{Code: broker.CodeMalformed, Field: "predicates", Msg: err.Error()})
		return
	}
	usage := d.usage.Read(d.ctx, false).Backends
	if usage == nil {
		usage = []PlanUsage{}
	}
	pred.Usage = usage
	pred.Now = d.clock.Now()
	if pred.Purpose != "" || pred.Model != "" || pred.Effort != "" {
		pred.Intel = &ModelIntelArgs{Dir: filepath.Join(d.stateDir, ModelIntelDirName), Now: pred.Now}
	}
	pick, err := Resolve(d.ctx, pred)
	if err != nil {
		_ = c.Fail(req.ID, err)
		return
	}
	raw, _ := EncodePickWire(pick)
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeResolved, Resolved: &broker.ResolveResponse{Pick: raw}})
}

func (d *BrokerDaemon) handleStatus(c *broker.ClientConn, req *broker.Request) {
	at := d.usage.Read(d.ctx, false).FetchedAt
	d.mu.Lock()
	tasks := len(d.tasks)
	d.mu.Unlock()
	_ = c.Reply(&broker.Response{ID: req.ID, Type: broker.TypeStatusResult, Status: &broker.StatusResponse{
		ProtocolVersion: broker.Version,
		Sessions:        []broker.SessionStatus{},
		Grants:          d.grantList(),
		Tasks:           tasks,
		UsageFetchedAt:  at,
	}})
}

// grantList is every registered seat with its ownership and liveness.
func (d *BrokerDaemon) grantList() []broker.GrantStatus {
	defs := d.reg.List()
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	out := make([]broker.GrantStatus, 0, len(defs))
	for _, def := range defs {
		st := broker.GrantStatus{
			Name: def.Name, Provider: broker.Provider(def.Provider), Model: def.Model,
			SessionID: def.SessionID, WorkDir: def.WorkDir, Purpose: def.Purpose, Parent: def.Parent,
		}
		if st.Provider == "" {
			st.Provider = broker.ProviderClaude
		}
		d.mu.Lock()
		if g := d.grants[def.Name]; g != nil {
			st.Owned = g.owner != nil
			st.Pending = len(g.ring)
			if g.proc != nil {
				st.Alive = g.proc.Alive()
				st.TurnCaps = turnCapsWire(g.proc)
			}
		}
		d.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// resumeSeats brings back every seat the daemon held before it last
// stopped. The work is the Registry's (ResumeAll), including attaching each
// seat's MCP to this process's host, whose loopback listener moved with the
// restart.
func (d *BrokerDaemon) resumeSeats() {
	if d.opts.resumeGate != nil {
		select {
		case <-d.opts.resumeGate:
		case <-d.ctx.Done():
			return
		}
	}
	held := 0
	for _, def := range d.reg.List() {
		if def.AutoStart {
			held++
		}
	}
	if held == 0 {
		return
	}
	d.log.Info("resuming seats held before the last stop", "count", held)
	outcomes := d.reg.ResumeAll(d.ctx, &ResumeArgs{
		Concurrency: d.opts.ResumeConcurrency,
		Nudge:       d.opts.RestartNudge,
		Now:         d.clock.Now(),
	})
	for _, o := range outcomes {
		if o.How == ResumeReminted {
			d.log.Warn("seat conversation unresumable; reminted on a fresh session",
				"grant", o.Name, "old_session", o.OldSessionID, "new_session", o.Agent.SessionID())
		}
		if o.NudgeErr != nil {
			d.log.Warn("restart nudge failed", "grant", o.Name, "err", o.NudgeErr)
		}
	}
}

// onSeatEvent forwards the Registry's seat lifecycle onto the tail, and
// takes hold of a resumed seat before it is nudged so the turn the nudge
// starts is retained for whoever reclaims the seat.
func (d *BrokerDaemon) onSeatEvent(ev SeatEvent) {
	switch ev.Kind {
	case SeatResumed:
		d.mu.Lock()
		g := d.grants[ev.Name]
		if g == nil {
			g = &brokerGrant{name: ev.Name}
			d.grants[ev.Name] = g
		}
		if g.proc != ev.Agent {
			d.bindSeatLocked(g, ev.Agent)
		}
		d.mu.Unlock()
		d.log.Info("seat resumed", "grant", ev.Name, "how", ev.How, "session", ev.SessionID)
		d.emit(broker.EventMessage{Kind: broker.EventResume, Name: ev.Name, SessionID: ev.SessionID, Detail: string(ev.How), At: ev.At})
	case SeatResumeFailed:
		d.log.Warn("seat resume failed", "grant", ev.Name, "err", ev.Err)
		d.emit(broker.EventMessage{Kind: broker.EventResumeFailed, Name: ev.Name, Detail: ev.Err.Error(), At: ev.At})
	case SeatNudged:
		d.emit(broker.EventMessage{Kind: broker.EventNudge, Name: ev.Name, At: ev.At})
	case SeatGone:
		const reason = "process not alive"
		d.log.Warn("seat process gone", "grant", ev.Name)
		d.mu.Lock()
		var owner *broker.ClientConn
		if g := d.grants[ev.Name]; g != nil && g.proc == ev.Agent {
			owner = g.owner
		}
		d.mu.Unlock()
		if owner != nil {
			_ = owner.Reply(&broker.Response{Type: broker.TypeAgentGone, AgentGone: &broker.AgentGoneMessage{Name: ev.Name, Reason: reason}})
		}
		d.emit(broker.EventMessage{Kind: broker.EventGone, Name: ev.Name, Detail: reason, At: ev.At})
	}
}
