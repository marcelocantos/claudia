// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package claudia embeds Claude Code agents in Go programs.
//
// It provides two modes of operation:
//
//   - Task mode: [Run] or [NewTask] + [Task.Run] sends a single prompt to
//     Claude Code and returns the result. Each prompt spawns a new process
//     with --output-format stream-json. Suitable for one-shot code
//     generation, analysis, or transformation pipelines where you want
//     structured event output and automatic token/cost accounting.
//
//   - Session mode: [Start] spawns a persistent Claude Code process inside
//     a tmux window on a dedicated claudia tmux server. Use [Agent.Send] to
//     send messages, [Agent.SubscribeEvents] to observe JSONL events, and
//     [Agent.WaitForResponse] to block until the next assistant turn
//     completes. The session persists across consumer restarts.
//
// A warm agent pool is available for both modes: [Acquire] checks out a
// pre-warmed [Agent] from the pool (identified by workdir, model, and
// allowed tools). Return it with [Agent.Release].
//
// The tmux substrate gives agents crash-survival (the agent stays alive if
// the consumer process dies) and human-attachable observability via
// [Agent.AttachCommand]. JSONL transcript tailing drives the event stream
// regardless of transport.
//
// The claude binary is located via the CLAUDE_BIN environment variable,
// then exec.LookPath, then common install dirs (~/.local/bin/claude,
// ~/.claude/local/claude, /opt/homebrew/bin/claude, /usr/local/bin/claude).
package claudia

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// Config configures a Claude Code agent.
type Config struct {
	// Provider selects the runtime backing this agent. Empty means
	// ProviderClaude. ProviderGrok uses ACP over `grok agent stdio`.
	// ProviderCodex Session mode uses `codex app-server` JSON-RPC.
	// ProviderCursor uses ACP over `agent acp`.
	Provider Provider

	// Name is the grant key when a claudia daemon holds the seat (🎯T2.10):
	// a consumer that restarts reclaims the running agent by this name
	// instead of starting another. Registry sets it to the AgentDef name.
	// Empty means an anonymous grant keyed by session id. Direct (no
	// daemon) Start does not use it.
	Name string

	// WorkDir is the working directory for the Claude Code process.
	// Defaults to ".".
	WorkDir string

	// SessionID is a persistent session ID. If empty, a new random
	// session is created. If non-empty and the session JSONL already
	// exists, the session is resumed with --resume.
	SessionID string

	// RequireResume marks SessionID as an existing conversation: a
	// failed resume/load is then a hard error — the provider must never
	// silently mint a replacement session (conversation loss). Leave
	// false for locally minted ids that have no conversation yet.
	RequireResume bool

	// Model overrides the default Claude model (e.g. "opus", "sonnet").
	Model string

	// PermissionMode sets the Claude Code permission mode.
	// Defaults to "bypassPermissions".
	PermissionMode string

	// SandboxMode selects the Codex app-server sandbox for
	// ProviderCodex Session (e.g. "read-only", "workspace-write").
	// Empty keeps the safe default of read-only (🎯T37). Other
	// providers refuse a non-empty value rather than drop it.
	//
	//
	// "workspace-write" makes the working directory writable and keeps
	// its `.git` read-only: the seat can edit its repo and cannot commit
	// to it. Start logs that when it applies. SandboxGitWrite lifts it.
	SandboxMode string

	// SandboxGitWrite lets a workspace-write Codex seat write its repo's
	// git directories (the shared one, for a linked worktree), so it can
	// `git commit` and `git worktree add` (🎯T109). Start fails, naming
	// the directory, if the app-server reports a sandbox without the
	// grant, and refuses the field outright on a read-only seat.
	//
	// It is off by default, and it is a real concession (🎯T112). Codex
	// protects `.git` because `.git/hooks` and `.git/config` are code
	// that runs outside the sandbox: a hook the seat plants executes
	// unsandboxed, as the operator, the next time anyone runs git in that
	// repository. Set it for a seat whose mission is to commit, in a
	// repository whose operator accepts that; leave it unset otherwise.
	SandboxGitWrite bool

	// SandboxWritableRoots and SandboxNetworkAccess widen a Codex
	// workspace-write sandbox beyond the working directory (🎯T598).
	//
	// These cannot ride thread/start. Its `sandbox` field is a UNIT
	// variant — "read-only" | "workspace-write" | "danger-full-access" —
	// and a map there is refused with "invalid type: map, expected unit".
	// The sibling `sandboxPolicy` param looks like the right home and is
	// worse than useless: the app-server accepts it and silently discards
	// it, a deliberately bogus value included, so a client that used it
	// would report success while the sandbox stayed read-only.
	//
	// The dimensions live in CODEX_HOME/config.toml
	// ([sandbox_workspace_write] writable_roots, network_access), which
	// claudia already owns per session, so they are written there.
	// Verified against codex-cli 0.148.0-alpha.9 on 2026-08-31.
	SandboxWritableRoots []string
	SandboxNetworkAccess bool

	// Goal is a durable host-owned objective for this Session. Empty
	// keeps one-shot Send. When set, the Agent issues a continuation
	// Send after each terminal assistant turn until Stop, Interrupt,
	// or an assistant line GOAL_STATUS: complete / blocked. The
	// string is not forwarded to any provider /goal RPC or slash
	// command, so a later Start on another Provider can carry the
	// same objective (🎯T39).
	Goal string

	// GoalCompleteCheck, when set, is consulted after a terminal turn
	// before a Goal continuation Send. Returning true closes the Goal
	// without injecting "Continue the open objective" — for hosts that
	// know mission completeness from an external ledger (jevons 🎯T528).
	// The check receives the durable Goal string and the settled turn text.
	GoalCompleteCheck func(goal, turnText string) bool

	// MCPConfig is the path to an MCP config JSON file.
	// Empty means Claude Code uses its default discovery.
	// Prefer [Config.MCPServers] plus [LoadMCP] so callers do not
	// maintain per-provider files (🎯T40).
	MCPConfig string

	// MCPServers is the session-scoped MCP list in Claudia's dialect.
	// This is the only way Claudia populates a backend's MCP set at
	// Start: Claude gets an inline/temp --mcp-config; Grok/Cursor get
	// ACP mcpServers; Codex gets a process-private CODEX_HOME persisted
	// under $XDG_STATE_HOME/claudia/codex-homes/<sessionID>. Claudia
	// never writes ~/.claude.json, ~/.grok, ~/.codex, ~/.cursor, or
	// project .cursor/mcp.json for Session attach.
	MCPServers []MCPServer

	// MCPExclusive, when true, is the only MCP set the Session may
	// see (🎯T45). Default false keeps provider user-scope maps
	// additive where the backend still loads them (Cursor has no
	// strict flag). Claude uses --strict-mcp-config; Grok uses a
	// durable per-session GROK_HOME under $XDG_STATE_HOME/claudia/grok-homes;
	// Codex uses a process-private
	// CODEX_HOME persisted under $XDG_STATE_HOME/claudia/codex-homes
	// so bounce can thread/resume (jevons 🎯T545.1.2).
	MCPExclusive bool

	// DisallowTools lists additional tool names to disallow. Agent,
	// SendMessage, and EnterWorktree are
	// always disallowed in addition to whatever appears here.
	DisallowTools []string

	// ExtraArgs are additional CLI arguments passed to claude.
	ExtraArgs []string

	// TermLogPath is the path to which raw terminal output (including
	// ANSI escapes) is appended. If empty, it defaults to
	// $XDG_STATE_HOME/claudia/terms/<escaped-workdir>/<sessionID>.term
	// (with $XDG_STATE_HOME defaulting to ~/.local/state). Set to "-"
	// to disable terminal logging.
	TermLogPath string

	// PoolPolicy controls what Acquire does when all matching pool
	// windows are held by other consumers.
	//   "spawn" (default): create a new window.
	//   "wait":            block until a window is released (currently
	//                      falls back to "spawn").
	//   "error":           return an error immediately.
	PoolPolicy string

	// PoolCap is the maximum number of pool windows (idle + held) for
	// this pool key. When Acquire would exceed the cap, the oldest idle
	// window is evicted. 0 means unlimited.
	PoolCap int

	// GrokConnect enables connect-mode for ProviderGrok Session: a
	// detached `grok agent serve` process with ACP over WebSocket so
	// the agent outlives the consumer (jevons 🎯T40). Also enabled when
	// CLAUDIA_GROK_CONNECT is truthy, or when ConnectURL is set.
	GrokConnect bool

	// ConnectURL is a prior serve WebSocket URL for reattach
	// (e.g. ws://127.0.0.1:PORT/ws?server-key=SECRET). Used with
	// ConnectPID when process is still alive.
	ConnectURL string

	// ConnectPID is the OS PID of the durable serve process for
	// reattach / Alive probes. 0 means unknown.
	ConnectPID int

	// TurnSilenceBound is how long [Agent.WaitForResponse] may sit with
	// nothing at all arriving from this agent — no event of any type, no
	// terminal byte — before it reports [ErrTurnAbandoned] instead of
	// waiting on a terminal event that is never coming (🎯T96). Zero
	// takes the package default (turnSilenceBound).
	//
	// It bounds silence, not the turn: any activity rearms it, so a turn
	// that runs for hours is unaffected. Raise it only for an agent whose
	// healthy turns really do go quiet for longer — a tool call that
	// neither prints nor reports progress for that long.
	TurnSilenceBound time.Duration
}

// Agent is a persistent Claude Code process running inside a tmux
// window on the dedicated claudia tmux server. The tmux substrate
// provides crash-survival (the agent stays alive if the consumer
// process dies) and human-attachable observability (see
// [Agent.AttachCommand]).
type Agent struct {
	provider     Provider
	sessionID    string
	jsonlPath    string
	termLogPath  string
	tmuxWindowID string
	tmuxCtrl     agentControl
	ops          agentOps

	// connect-mode (Grok serve): durable process endpoint for upgrade reattach.
	connectURL string
	connectPID int

	// brokerGrant is the daemon grant name when this handle is a socket
	// client of a seat the daemon owns (🎯T2.10). Empty on the direct path.
	brokerGrant string
	// termSubscribed records that the daemon was asked to stream terminal
	// bytes for this seat (once per handle).
	termSubscribed bool
	// onSubscribe runs once, on the first SubscribeEvents: the broker
	// backend holds a reclaim's replayed history until the consumer has
	// attached a subscriber (or a short grace elapses), so a consumer that
	// Starts and then subscribes sees what the seat said while unowned.
	onSubscribe func()

	// mcpCleanup removes process-private MCP materialisation created at Start.
	mcpCleanup func()

	mu    sync.Mutex
	alive bool
	// dead is closed when alive goes false, so a WaitForResponse whose
	// turn can no longer be answered ends at the death instead of
	// waiting on an event nothing will publish (🎯T96). Created on
	// demand by deadSignal; nil until somebody waits.
	dead      chan struct{}
	stopOnce  sync.Once
	eventSubs map[int64]EventFunc
	turn      turnLatch
	usage     Usage
	model     string // resolved model from the latest event that carried one

	// clk is the time source WaitForResponse's silence bound reads, so a
	// hermetic test drives the bound with a ManualClock instead of
	// sleeping — the bound's verdict must not depend on how fast the
	// host is (🎯T33/🎯T92/🎯T93). Nil reads the wall clock.
	clk Clock
	// turnSilenceBound is Config.TurnSilenceBound; 0 takes the package
	// default.
	turnSilenceBound time.Duration

	// poolWindow is true when the agent was acquired from the warm pool
	// (via Acquire) rather than spawned fresh (via Start). Pool agents
	// must be released via Release rather than stopped with Stop.
	poolWindow bool
	// poolWorkDir is the resolved absolute working directory used as
	// part of the pool key. Set by buildPoolAgent; empty for Start agents.
	poolWorkDir string

	// windowAliveFn probes whether this agent's tmux window still exists
	// (🎯T602). Set by the tmux backend at start; nil everywhere else,
	// which is what keeps hermetic fixtures — and every non-tmux
	// provider — out of the probe. A synthetic window id is not a claim
	// that tmux knows about it.
	windowAliveFn func(string) bool
	windowCheckAt time.Time
	windowCheckOK bool

	// Host-owned goal loop (🎯T39). goal is copied from Config at Start.
	goal              string
	goalClosed        bool
	goalSeenTerminal  bool
	goalTimer         *time.Timer
	goalTurn          strings.Builder
	goalCompleteCheck func(goal, turnText string) bool

	// goalTurnWorked: the turn now accumulating has called a tool.
	// goalIdleTurns: consecutive terminal turns that did not (🎯T110).
	goalTurnWorked bool
	goalIdleTurns  int

	// startCfg is the Config Start resolved (workdir, MCP, Goal, …) so
	// Migrate can spawn the destination without the caller restating it.
	startCfg Config
	// backendGen increments on Migrate so source control/JSONL/TUI
	// goroutines do not mark the destination dead when they exit.
	backendGen atomic.Uint64
	// onMigrated is set by the Registry that launched this agent (🎯T75.3):
	// it runs once the handle names the destination, so the persisted
	// definition follows the seat. Guarded by mu.
	onMigrated func()
	// inertTurns is the bounded live-turn log Migrate distills (🎯T55.1).
	inertTurns []inertTurn

	// Terminal output streaming. termMu also guards termLog writes,
	// termLog close, and termLogLive so Stop cannot close the file
	// while pushTermOutput is mid-write.
	termMu  sync.Mutex
	termBuf []byte
	// termActivityAt is when the last terminal byte arrived: a working
	// TUI repaints, so it is turn activity for the silence bound even
	// while the transcript says nothing (🎯T96).
	termActivityAt time.Time
	termSubs       []chan []byte
	termLog        *os.File
	termLogLive    bool // false once the log file has been closed or failed to open

	// TUI readiness. ready closes once detectReady concludes, either
	// because the capture-pane regex matched (success, readyErr == nil)
	// or because detection gave up (failure, readyErr set). Send
	// blocks on this channel before writing to the agent.
	ready    chan struct{}
	readyErr error

	// Claude Session provisional ⏺ preview (🎯T51). Nil on non-Claude
	// backends. capturePane is injectable for hermetic pane fixtures.
	tuiPreviewMu sync.Mutex
	tuiPreview   *tuiPreviewTracker
	capturePane  func() (string, error)
}

// nextEventSubID is a process-wide counter for event subscription tokens.
var nextEventSubID atomic.Int64

// Readiness detection tuning.
const (
	// readyOverallTimeout is how long a cold Claude seat may take to draw
	// a live composer before Start reports it not ready.
	//
	// The number is measured, not chosen (🎯T108, cmd/t108ready,
	// 2026-09-21). Cold readiness is dominated by how busy this host is:
	// the pane stays blank until Claude Code paints its banner and
	// composer in a single frame, then the splash clears. From spawn to a
	// live composer, bucketed by the 1-minute load average at spawn and at
	// ready (a sample is in the high bucket if either reading was):
	//
	//	load 200-249  15.6s 19.8s 20.9s 23.0s 23.3s
	//	load 250-480  34.3s 45.7s 52.8s 53.1s 54.1s 55.9s 57.0s 63.7s
	//
	// The old 30s was set against a quieter host; it refused every one
	// of the eight high samples, and this fleet spends hours above 250.
	// Two minutes is 1.9x the worst healthy sample. A false positive
	// silently removes a working seat from the fleet, while a true wedge
	// costs only the extra wait, so the generous side is the correct
	// side to err on — and a timeout now says which it was (still_drawing,
	// not_started, window_gone or no_composer; see tmuxagent.waitVerdict).
	//
	// One sample, at load 583, never went live: it sat on the splash for
	// the remaining 126s of a 180s probe. No bound fits that host; the
	// timeout names it splash.
	readyOverallTimeout = 2 * time.Minute
	readyPollInterval   = 50 * time.Millisecond
)

// checkTmux returns a clear error if tmux is not on PATH.
func checkTmux() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux is required for claudia Session mode but was not found on PATH; install it via: brew install tmux (macOS) or apt install tmux (Linux)")
	}
	return nil
}

type agentControl interface {
	Bytes() <-chan []byte
	Close() error
}

type agentOps struct {
	attachCommand func(*Agent) string
	interrupt     func(*Agent) error
	send          func(*Agent, string) error
	resize        func(*Agent, uint16, uint16) error
	stop          func(*Agent)
	// reclaim is set only by the broker backend: it makes sure this
	// handle's connection owns the seat's grant on the daemon (🎯T124).
	reclaim func() error
	// promptInFlight is optional (Grok ACP). Nil → always false.
	promptInFlight func(*Agent) bool
	// setModel switches the in-session model within the same provider
	// (🎯T54). Nil → SetModel refuses after the capability check.
	setModel func(*Agent, string) error
	// migrate is set only by the broker backend: the daemon performs the
	// provider swap and this handle re-points at the destination. Nil →
	// Migrate runs the in-process swap.
	migrate func(*Agent, *MigrateArgs) error
	// rewind is set only by the broker backend: the daemon rolls the seat
	// back and relaunches it, and this handle re-points (🎯T75.8).
	rewind func(*Agent, int) (*RewindResult, error)
	// droppedFrames is set only by the broker backend: how many frames its
	// connection has skipped as too large to relay, and the last refusal
	// (🎯T73). WaitForResponse compares the count across a wait, so a
	// silence that may be a lost terminal event says so (🎯T105).
	droppedFrames func(*Agent) (int, error)
	// release is set only by the broker backend for an acquired seat: the
	// daemon returns it to, or drops it from, the pool it runs (🎯T64).
	release func(*Agent, string) error
	// subscribeTerminal is set only by the broker backend: the first
	// SubscribeTerminal asks the daemon to stream raw bytes.
	subscribeTerminal func(*Agent)
	// closeGoal is set only by the broker backend: CloseGoal tells the
	// daemon to stop continuing the seat's Goal.
	closeGoal func(*Agent)
	// steer folds text into the open turn (🎯T72.2). Nil → Steer reports
	// ErrSteerUnsupported and TurnCaps withdraws the steer claim. Direct
	// backends set it with steerOp(client); the broker handle forwards
	// mode=steer over the wire and fills the outcome from the response.
	steer func(*Agent, string) (DeliveryOutcome, error)
	// turnCaps refines the provider contract for this handle (a Codex CLI
	// without turn/steer; the daemon's answer for a broker seat). Nil →
	// ProviderTurnCaps.
	turnCaps func(*Agent) TurnCaps
	// turnPhase reports the open-turn phase. Nil → derived from
	// promptInFlight.
	turnPhase func(*Agent) TurnPhase
}

type agentStartRequest struct {
	Context         context.Context
	Config          Config
	WorkDir         string
	SessionID       string
	JSONLPath       string
	TermLogPath     string
	Resuming        bool
	DisallowedTools string
}

type agentStart struct {
	WindowID string
	// WindowAlive probes whether WindowID still exists (🎯T602). Only a
	// backend that really created a tmux window sets it; a fixture with a
	// synthetic WindowID leaves it nil and keeps the old flag-only
	// liveness, which is what keeps hermetic tests hermetic.
	WindowAlive          func(string) bool
	Control              agentControl
	Ops                  agentOps
	TailJSONL            bool
	StoreSessionInWindow bool
	DetectReady          func(*Agent)
	// SessionID, when non-empty, replaces the pre-assigned Agent.sessionID
	// (used when the provider allocates the id, e.g. Grok ACP session/new).
	SessionID string
	// JSONLPath, when non-empty, replaces the Claude-shaped transcript path.
	JSONLPath string
	// ConnectURL / ConnectPID for Grok connect-mode (durable serve).
	ConnectURL string
	ConnectPID int
	// Cleanup removes process-private MCP materialisation (temp --mcp-config
	// files, ephemeral GROK_HOME). Codex exclusive homes persist under
	// XDG state so Stop does not delete the rollout. Called from Agent.Stop.
	Cleanup func()
	// GrantName marks a broker-held seat and names it (🎯T2.10).
	GrantName string
	// TermLogPath, when non-empty, is the daemon's terminal log for a
	// broker-held seat: reported by TermLogPath, never written by this
	// process.
	TermLogPath string
}

type agentBackend interface {
	Capabilities() providerCapabilities
	StartAgent(agentStartRequest) (*agentStart, error)
}

type claudeAgentBackend struct{}

type codexAgentBackend struct{}

type grokAgentBackend struct{}

type cursorAgentBackend struct{}

type errorAgentBackend struct {
	err error
}

func agentBackendForProvider(provider Provider) agentBackend {
	switch provider {
	case "", ProviderClaude:
		return claudeAgentBackend{}
	case ProviderCodex:
		return codexAgentBackend{}
	case ProviderGrok:
		return grokAgentBackend{}
	case ProviderCursor:
		return cursorAgentBackend{}
	case ProviderBedrock:
		return errorAgentBackend{err: CheckCapability(ProviderBedrock, CapabilitySession)}
	default:
		return errorAgentBackend{err: fmt.Errorf("unknown agent provider %q", provider)}
	}
}

func (claudeAgentBackend) Capabilities() providerCapabilities {
	return claudeProviderCapabilities()
}

func (codexAgentBackend) Capabilities() providerCapabilities {
	return providerCapabilities{
		Task:    true,
		Resume:  true,
		Session: true,
	}
}

func (codexAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	return startCodexAgent(req)
}

func (grokAgentBackend) Capabilities() providerCapabilities {
	return providerCapabilities{
		Task:    true,
		Session: true,
		Resume:  true,
	}
}

func (grokAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	return startGrokAgent(req)
}

func (cursorAgentBackend) Capabilities() providerCapabilities {
	return providerCapabilities{
		Session: true,
		Resume:  true,
	}
}

func (cursorAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	return startCursorAgent(req)
}

func (b errorAgentBackend) Capabilities() providerCapabilities {
	return providerCapabilities{}
}

func (b errorAgentBackend) StartAgent(agentStartRequest) (*agentStart, error) {
	return nil, b.err
}

func claudeAgentOps() agentOps {
	return agentOps{
		// 🎯T601: a tmux Claude session must answer this from the pane.
		// Leaving it unset made Agent.PromptInFlight() return a confident
		// false for every Claude session — not "unknown", but "no turn is
		// running" while one visibly was. A caller using that to decide
		// whether an agent is wedged concludes the wrong thing precisely
		// when a turn is long, which is when it matters.
		promptInFlight: func(a *Agent) bool {
			if a == nil || a.tmuxWindowID == "" {
				return false
			}
			frame, err := tmuxagent.CapturePane(a.tmuxWindowID)
			if err != nil {
				return false
			}
			return tmuxagent.MatchTurnInProgress(frame)
		},
		attachCommand: func(a *Agent) string {
			return fmt.Sprintf("tmux -S %s attach -t %s", tmuxagent.SocketPath(), a.tmuxWindowID)
		},
		interrupt: func(a *Agent) error {
			return tmuxagent.SendEscape(a.tmuxWindowID)
		},
		send: func(a *Agent, msg string) error {
			return tmuxagent.SendKeys(a.tmuxWindowID, msg)
		},
		resize: func(a *Agent, cols, rows uint16) error {
			return tmuxagent.ResizeWindow(a.tmuxWindowID, cols, rows)
		},
		stop: func(a *Agent) {
			tmuxagent.KillWindow(a.tmuxWindowID)
			if a.tmuxCtrl != nil {
				a.tmuxCtrl.Close()
			}
		},
		setModel: func(a *Agent, model string) error {
			// Claude Code: /model <alias|id> switches the live session model.
			return tmuxagent.SendKeys(a.tmuxWindowID, "/model "+model)
		},
	}
}

// Start spawns a new agent for cfg.Provider. Claude uses a tmux-backed
// Session; Grok uses ACP over `grok agent stdio`; Cursor uses ACP over
// `agent acp`; Codex uses `codex app-server` JSON-RPC.
func Start(cfg Config) (*Agent, error) {
	return StartContext(context.Background(), cfg)
}

// StartContext starts an agent with a cancelable startup. The context does not
// own the returned agent's lifetime. Cursor interrupts ACP startup immediately;
// cancellation in other backends remains cooperative and may only be observed
// before their provider operation begins. Registry Stop/Remove join any pending
// startup and clean up its result before completing.
func StartContext(ctx context.Context, cfg Config) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Gate on the published capability matrix rather than a per-provider
	// branch list, so Start cannot drift into offering a session claudia
	// has not claimed. Unknown providers still fall through to the
	// backend's own "unknown provider" error.
	if _, known := providerCapabilityClaims[cfg.Provider]; known || cfg.Provider == "" {
		if err := CheckCapability(cfg.Provider, CapabilitySession); err != nil {
			return nil, err
		}
	}
	return startConsideringBrokerContext(ctx, cfg, agentBackendForProvider(cfg.Provider))
}

// startDirectContext is StartContext without the broker consult: the path
// the daemon itself takes to start a provider process.
// StartDirect is [Start] in this process even when a claudia daemon is
// listening. Unlike CLAUDIA_NO_BROKER it affects only this call and is not
// inherited by the processes the agent starts.
func StartDirect(cfg Config) (*Agent, error) {
	return startDirectContext(context.Background(), cfg)
}

// StartDirectContext is [StartDirect] with cooperative startup cancellation.
func StartDirectContext(ctx context.Context, cfg Config) (*Agent, error) {
	return startDirectContext(ctx, cfg)
}

// DaemonHeld reports whether a claudia daemon holds this seat, so this
// Agent is a handle onto a process the daemon parents.
func (a *Agent) DaemonHeld() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.brokerGrant != ""
}

// Detach lets go of a daemon-held seat without stopping it: the seat keeps
// running unowned, keeps what it says for whoever reclaims it, and a later
// Start or Registry launch with the same Config.Name reclaims it. This
// handle stops reporting alive. It is how a consumer restarts or upgrades
// without bouncing its agents. An agent this process started is not
// daemon-held, and Detach refuses it: stopping this process ends it.
func (a *Agent) Detach() error {
	if !a.DaemonHeld() {
		return fmt.Errorf("Detach: agent is not held by a claudia daemon")
	}
	if a.mcpCleanup != nil {
		// A broker handle's cleanup closes its connection, which the
		// daemon treats as the owner going away, not as a release.
		a.mcpCleanup()
	}
	return nil
}

// ensureOwned makes sure a daemon-held seat's grant belongs to this
// handle's connection, re-claiming it after a detach. Nil for a seat that is
// not daemon-held.
func (a *Agent) ensureOwned() error {
	if a.ops.reclaim == nil {
		return nil
	}
	return a.ops.reclaim()
}

func startDirectContext(ctx context.Context, cfg Config) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, known := providerCapabilityClaims[cfg.Provider]; known || cfg.Provider == "" {
		if err := CheckCapability(cfg.Provider, CapabilitySession); err != nil {
			return nil, err
		}
	}
	return startWithBackendContext(ctx, cfg, agentBackendForProvider(cfg.Provider))
}

func startWithBackend(cfg Config, backend agentBackend) (*Agent, error) {
	return startWithBackendContext(context.Background(), cfg, backend)
}

func startWithBackendContext(ctx context.Context, cfg Config, backend agentBackend) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	provider := cfg.Provider
	if provider == "" {
		provider = ProviderClaude
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "."
	}
	workDir, _ := filepath.Abs(cfg.WorkDir)
	// Resolve symlinks so our project-dir escaping matches Claude
	// Code's own canonicalisation. On macOS, /var is a symlink to
	// /private/var, and any workdir under /var/folders (including
	// Go's t.TempDir()) produces a JSONL transcript under
	// -private-var-folders-..., while our unresolved path escapes
	// to -var-folders-... — we'd tail a file Claude never writes.
	// If resolution fails (path missing, permission denied) we fall
	// back to the unresolved Abs path rather than failing Start.
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}

	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "bypassPermissions"
	}

	sessionID := cfg.SessionID
	// Codex Session identity is the app-server thread id, assigned on
	// thread/start. Minting a UUID here would make the first Start try
	// to resume a fake id, and the live CLI does not use a thr_ prefix.
	if sessionID == "" && provider != ProviderCodex {
		sessionID = uuid.New().String()
	}
	jsonlPath := SessionJSONLPath(sessionID, workDir)

	termLogPath := cfg.TermLogPath
	switch termLogPath {
	case "":
		termLogPath = filepath.Join(termLogDir(workDir), sessionID+".term")
	case "-":
		termLogPath = ""
	}

	// If the Claude JSONL already exists, this is a resume. Grok/Codex do
	// not use ~/.claude/projects transcripts; their resume semantics live
	// in the provider backend (Grok ACP session/load, etc.).
	resuming, err := SessionExists(sessionID, workDir)
	if err != nil {
		return nil, fmt.Errorf("check session JSONL: %w", err)
	}
	// FAIL-CLOSED RESUME (Claude): when the caller marked this id as an
	// existing conversation (RequireResume), missing JSONL must be a hard
	// error — silently falling through to --session-id would mint a
	// replacement conversation and orphan the caller's history (Registry
	// Materialized sets RequireResume only after real conversation
	// evidence, not bare Start success).
	// Grok enforces the same policy inside startGrokACP / session/load.
	if (provider == ProviderClaude) && cfg.RequireResume && !resuming {
		return nil, fmt.Errorf("session %s: existing conversation required but JSONL not found at %s — refusing to mint a replacement session", sessionID, jsonlPath)
	}
	// A resumed transcript already holds the conversation being resumed.
	// Tailing it from byte zero republished every earlier turn as if the
	// relaunched seat had just said it: a daemon that rewinds a seat binds
	// the owner's stream to the relaunch after it starts, so the surviving
	// turn's "ok" reached the owner as the answer to its next question
	// (🎯T114). The end is measured before the relaunch appends anything,
	// so every line the resumed seat writes is still published. A pooled
	// window does the same for a previous holder's turns (🎯T78).
	var tailFrom int64
	if resuming {
		if fi, err := os.Stat(jsonlPath); err == nil {
			tailFrom = fi.Size()
		}
	}

	disallowed := disallowedToolList(cfg.DisallowTools)

	a := &Agent{
		provider:          provider,
		sessionID:         sessionID,
		jsonlPath:         jsonlPath,
		termLogPath:       termLogPath,
		alive:             true,
		ready:             make(chan struct{}),
		eventSubs:         make(map[int64]EventFunc),
		goal:              strings.TrimSpace(cfg.Goal),
		goalCompleteCheck: cfg.GoalCompleteCheck,
		startCfg:          cfg,
		model:             cfg.Model,
		clk:               SystemClock{},
		turnSilenceBound:  cfg.TurnSilenceBound,
	}

	// Open terminal log.
	if termLogPath != "" {
		if err := os.MkdirAll(filepath.Dir(termLogPath), 0o755); err != nil {
			slog.Warn("term log mkdir failed", "path", termLogPath, "err", err)
		} else if f, err := os.OpenFile(termLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			slog.Warn("term log open failed", "path", termLogPath, "err", err)
		} else {
			a.termLog = f
			a.termLogLive = true
		}
	}

	start, err := backend.StartAgent(agentStartRequest{
		Context:         ctx,
		Config:          cfg,
		WorkDir:         workDir,
		SessionID:       sessionID,
		JSONLPath:       jsonlPath,
		TermLogPath:     termLogPath,
		Resuming:        resuming,
		DisallowedTools: disallowed,
	})
	if err != nil {
		a.Stop()
		return nil, err
	}
	// A backend that returns neither a session nor an error is a bug in
	// that backend, but the cost of trusting it is a nil dereference
	// several lines below, well away from the cause.
	if start == nil {
		a.Stop()
		return nil, fmt.Errorf("%s agent backend returned no session and no error", provider)
	}
	if start.SessionID != "" {
		a.sessionID = start.SessionID
		sessionID = start.SessionID
	}
	if start.JSONLPath != "" {
		a.jsonlPath = start.JSONLPath
		jsonlPath = start.JSONLPath
	}
	windowID := start.WindowID
	a.tmuxWindowID = windowID
	a.windowAliveFn = start.WindowAlive
	a.tmuxCtrl = start.Control
	a.ops = start.Ops
	a.connectURL = start.ConnectURL
	a.connectPID = start.ConnectPID
	a.mcpCleanup = start.Cleanup
	a.brokerGrant = start.GrantName
	if start.TermLogPath != "" {
		a.termMu.Lock()
		a.termLogPath = start.TermLogPath
		a.termLogLive = true
		a.termMu.Unlock()
	}

	// Store session ID on the window for crash-survival recovery.
	if start.StoreSessionInWindow {
		if err := tmuxagent.SetWindowOption(windowID, "claudia-session-id", sessionID); err != nil {
			slog.Warn("failed to set session ID on tmux window", "err", err)
		}
	}

	attach := ""
	if a.ops.attachCommand != nil {
		attach = a.AttachCommand()
	}
	slog.Info("claudia agent started",
		"session", sessionID,
		"window", windowID,
		"attach", attach)

	// Register this session as the start of a new chain. The chain ID
	// equals the session ID for freshly-started agents. /clear detection
	// (which links subsequent sessions to the same chain) is deferred to
	// a follow-up target.
	if err := RegisterChain(sessionID, sessionID); err != nil {
		slog.Warn("failed to register session chain", "session", sessionID, "err", err)
	}

	if start.Control != nil {
		gen := a.backendGen.Load()
		go func() {
			for data := range start.Control.Bytes() {
				a.pushTermOutput(data)
			}
			slog.Debug("terminal control stream closed", "session", sessionID)
			if a.backendGen.Load() != gen {
				return
			}
			a.markDead()
		}()
	}

	if start.TailJSONL {
		go a.tailJSONLFrom(tailFrom)
	}
	// Claude Session: poll capture-pane for provisional ⏺ preview (🎯T51).
	if start.TailJSONL && windowID != "" {
		a.tuiPreview = &tuiPreviewTracker{}
		a.capturePane = func() (string, error) {
			b, err := tmuxagent.CapturePane(a.tmuxWindowID)
			if err != nil {
				return "", err
			}
			return string(b), nil
		}
		go a.pollTUIPreview()
	}

	if start.DetectReady != nil {
		// Synchronous: Grok ACP needs agentRef wired before Send, and
		// Claude readiness still runs its poll loop from inside DetectReady
		// as a nested goroutine where needed.
		start.DetectReady(a)
	} else {
		close(a.ready)
	}

	return a, nil
}

// claudeAgentArgs builds the argv for a tmux-backed Claude Code session.
// Kept separate from StartAgent so the request-field audit can materialise
// the request without a tmux server (see capability_audit_test.go).
func claudeAgentArgs(req agentStartRequest) []string {
	return claudeAgentArgsWithMCP(req, claudeMCPConfigArg(req))
}

func claudeAgentArgsWithMCP(req agentStartRequest, mcpConfig string) []string {
	args := []string{
		"--permission-mode", req.Config.PermissionMode,
		"--disallowedTools", req.DisallowedTools,
	}
	if req.Resuming {
		args = append(args, "--resume", req.SessionID)
	} else {
		args = append(args, "--session-id", req.SessionID)
	}
	if mcpConfig != "" {
		args = append(args, "--mcp-config", mcpConfig)
	}
	if req.Config.MCPExclusive {
		args = append(args, "--strict-mcp-config")
	}
	if req.Config.Model != "" {
		args = append(args, "--model", req.Config.Model)
	}
	return append(args, req.Config.ExtraArgs...)
}

// sandboxPolicyRequested reports whether the caller asked for ANY Codex
// sandbox tuning. Providers that cannot honour it must refuse rather than
// drop it: a silently ignored sandbox request is how a seat comes up with
// less access than its mission needs and fails only at its first gate
// (🎯T598).
func sandboxPolicyRequested(c Config) bool {
	return c.SandboxMode != "" || len(c.SandboxWritableRoots) > 0 || c.SandboxNetworkAccess || c.SandboxGitWrite
}

func claudeSessionPrecheck(req agentStartRequest) error {
	if sandboxPolicyRequested(req.Config) {
		return capabilityRefusal(ProviderClaude, CapabilitySandboxPolicy, sandboxPolicyIsCodexOnlyReason)
	}
	return nil
}

func (claudeAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	if err := claudeSessionPrecheck(req); err != nil {
		return nil, err
	}
	if err := checkTmux(); err != nil {
		return nil, err
	}

	mcpArg, mcpCleanup, err := prepareClaudeMCPConfig(req.Config)
	if err != nil {
		return nil, fmt.Errorf("session mcp: %w", err)
	}
	args := claudeAgentArgsWithMCP(req, mcpArg)

	windowName := tmuxagent.SessionWindowName(req.SessionID)
	claudeBin, err := resolveClaudeBin()
	if err != nil {
		if mcpCleanup != nil {
			mcpCleanup()
		}
		return nil, err
	}
	windowID, err := tmuxagent.SpawnWindow(req.WorkDir, windowName, claudeBin, args)
	if err != nil {
		if mcpCleanup != nil {
			mcpCleanup()
		}
		return nil, fmt.Errorf("tmux spawn: %w", err)
	}

	start, err := attachClaudeWindow(windowID)
	if err != nil {
		tmuxagent.KillWindow(windowID)
		if mcpCleanup != nil {
			mcpCleanup()
		}
		return nil, err
	}
	start.Cleanup = mcpCleanup
	return start, nil
}

// ErrNoSessionWindow is returned by [Adopt] when no live tmux window
// belongs to the session. Callers that want drain semantics fall back
// to [Start]; upgrade callers treat it as "this agent actually exited."
var ErrNoSessionWindow = errors.New("no live tmux window for session")

// Adopt rebuilds an [Agent] handle for a still-running Claude tmux
// window (or a Grok connect-mode serve). It does not spawn. Missing
// process is [ErrNoSessionWindow], not a silent Start.
func Adopt(cfg Config) (*Agent, error) {
	provider := cfg.Provider
	if provider == "" {
		provider = ProviderClaude
	}
	if _, known := providerCapabilityClaims[provider]; known || cfg.Provider == "" {
		if err := CheckCapability(cfg.Provider, CapabilitySession); err != nil {
			return nil, err
		}
	}
	if provider == ProviderGrok && (cfg.ConnectURL != "" || cfg.ConnectPID > 0) {
		// Grok reuse is Start with a live connect endpoint; it dials,
		// it does not mint a serve process.
		return Start(cfg)
	}
	if provider != ProviderClaude && cfg.Provider != "" {
		return nil, fmt.Errorf("%w: provider %s has no adopt path", ErrNoSessionWindow, provider)
	}
	return startWithBackend(cfg, adoptClaudeBackend{})
}

type adoptClaudeBackend struct{}

func (adoptClaudeBackend) Capabilities() providerCapabilities {
	return claudeProviderCapabilities()
}

func (adoptClaudeBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	if err := checkTmux(); err != nil {
		return nil, err
	}
	if req.SessionID == "" {
		return nil, ErrNoSessionWindow
	}
	ids, err := tmuxagent.WindowsForSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSessionWindow, req.SessionID)
	}
	return attachClaudeWindow(ids[0])
}

func attachClaudeWindow(windowID string) (*agentStart, error) {
	if !tmuxagent.IsWindowAlive(windowID) {
		return nil, fmt.Errorf("%w: window %s gone", ErrNoSessionWindow, windowID)
	}
	ctrl, err := tmuxagent.DialControl(windowID)
	if err != nil {
		return nil, fmt.Errorf("tmux control-mode: %w", err)
	}
	return &agentStart{
		WindowID:             windowID,
		WindowAlive:          tmuxagent.IsWindowAlive,
		Control:              ctrl,
		Ops:                  claudeAgentOps(),
		TailJSONL:            true,
		StoreSessionInWindow: true,
		DetectReady: func(a *Agent) {
			go a.detectReady()
		},
	}, nil
}

// grokSessionPlan is everything the Grok Session path derives from a
// start request before it launches anything. Splitting it out lets the
// request-field audit materialise the request without a grok binary, so
// a field that stops reaching the process is caught by a test rather than
// by whoever notices the behaviour missing.
type grokSessionPlan struct {
	// Args is the argv after the grok binary.
	Args    []string
	WorkDir string
	Model   string
	// PreferSessionID is the id offered to session/load; empty means
	// session/new.
	PreferSessionID string
	RequireResume   bool
	MCPServers      []any
	Connect         bool
	GrokHome        string
}

func planGrokSession(req agentStartRequest) grokSessionPlan {
	// Prefer caller SessionID when resuming; empty means session/new.
	preferID := ""
	if req.Resuming || req.Config.SessionID != "" {
		preferID = req.SessionID
	}
	connect := grokConnectEnabled(req.Config)
	home := ""
	if req.Config.MCPExclusive {
		// Audit sentinel; Start resolves the durable per-session GROK_HOME.
		home = "session:GROK_HOME"
	}
	return grokSessionPlan{
		Args:            grokACPArgs(req.Config.Model, connect),
		WorkDir:         req.WorkDir,
		Model:           req.Config.Model,
		PreferSessionID: preferID,
		RequireResume:   req.Config.RequireResume,
		MCPServers:      resolveACPMCPServers(req.Config),
		Connect:         connect,
		GrokHome:        home,
	}
}

// grokSessionPrecheck refuses a start request carrying a field the Grok
// Session path cannot honour.
//
// 🎯T24: all three used to be dropped in silence. DisallowTools is the
// same fail-open 🎯T23 closed for Grok Task — Config.DisallowTools never
// reached the ACP client, so a caller who stripped tools from an agent got
// one with every tool present. PermissionMode is worse in kind, because
// the value that survives is the most permissive one: the path hardcodes
// always-approve/yoloMode, so asking for "plan" or "acceptEdits" produced
// an agent that approved everything.
func grokSessionPrecheck(req agentStartRequest) error {
	if len(req.Config.DisallowTools) > 0 {
		return capabilityRefusal(ProviderGrok, CapabilityToolRestrictions,
			grokSessionToolRestrictionsUnwiredReason)
	}
	// startWithBackend defaults an empty PermissionMode to
	// bypassPermissions, which is what the path already does, so only a
	// caller asking for something stricter is refused.
	if mode := req.Config.PermissionMode; mode != "" && mode != "bypassPermissions" {
		return capabilityRefusal(ProviderGrok, CapabilityPermissionMode, grokPermissionModeReason)
	}
	if len(req.Config.ExtraArgs) > 0 {
		return capabilityRefusal(ProviderGrok, CapabilityExtraArgs, grokExtraArgsReason)
	}
	if sandboxPolicyRequested(req.Config) {
		return capabilityRefusal(ProviderGrok, CapabilitySandboxPolicy, sandboxPolicyIsCodexOnlyReason)
	}
	return nil
}

func codexSessionPrecheck(req agentStartRequest) error {
	if len(req.Config.DisallowTools) > 0 {
		return capabilityRefusal(ProviderCodex, CapabilityToolRestrictions, codexToolRestrictionsReason)
	}
	if mode := req.Config.PermissionMode; mode != "" && mode != "bypassPermissions" {
		return capabilityRefusal(ProviderCodex, CapabilityPermissionMode,
			"Codex sandbox/approval flags are Codex-native and are not proven equivalent to Claude PermissionMode")
	}
	if len(req.Config.ExtraArgs) > 0 {
		return capabilityRefusal(ProviderCodex, CapabilityExtraArgs,
			"Codex Session speaks typed app-server fields; Config.ExtraArgs have nowhere to go")
	}
	// A git grant on a read-only seat would be dropped: the writable root
	// is a workspace-write setting and nothing else reads it (🎯T112).
	if mode := resolveCodexSandbox(req.Config.SandboxMode); req.Config.SandboxGitWrite && mode == defaultCodexSandbox {
		return fmt.Errorf("codex: SandboxGitWrite asks for a writable .git but SandboxMode is %q, which writes nothing — set SandboxMode %q", mode, codexSandboxWorkspaceWrite)
	}
	return nil
}

// acpBind wires an ACP/app-server client to an Agent after Start
// constructs it. onClose is generation-gated so a source Close after
// [Agent.Migrate] cannot mark the destination dead (🎯T55).
type acpBind struct {
	ref atomic.Pointer[Agent]
	gen atomic.Uint64
}

func (b *acpBind) onEvent(ev Event) {
	if a := b.ref.Load(); a != nil {
		a.publishEvent(ev)
	}
}

func (b *acpBind) onClose() {
	if a := b.ref.Load(); a != nil {
		if a.backendGen.Load() != b.gen.Load() {
			return
		}
		a.markDead()
	}
}

func (b *acpBind) attach(a *Agent) {
	b.gen.Store(a.backendGen.Load())
	b.ref.Store(a)
}

func startCodexAgent(req agentStartRequest) (*agentStart, error) {
	if err := codexSessionPrecheck(req); err != nil {
		return nil, err
	}
	if _, err := ensureCodexSubscriptionAuth(nil); err != nil {
		return nil, err
	}
	bin, err := resolveCodexBin()
	if err != nil {
		return nil, err
	}
	// The steer claim is per install: only a CLI whose app-server schema
	// lists turn/steer gets the mechanism wired (🎯T72.2).
	steerSupported := codexAppServerSupportsSteer(bin)

	sandboxTuning, err := codexSandboxTuningFor(req)
	if err != nil {
		return nil, err
	}

	var bind acpBind

	var extraEnv []string
	var mcpCleanup func()
	var exclusiveHome string
	if needsSessionMCPMaterialization(req.Config) {
		home, cleanup, herr := exclusiveCodexHomeForStart(req.SessionID, req.Config.RequireResume, mergeMCPServers(req.Config), sandboxTuning)
		if herr != nil {
			return nil, herr
		}
		exclusiveHome = home
		mcpCleanup = cleanup
		extraEnv = exclusiveEnv("CODEX_HOME", home)
	}
	client, err := startCodexAppServer(bin, req.WorkDir, req.Config.Model, req.SessionID, req.Config.RequireResume, req.Config.SandboxMode,
		sandboxTuning, extraEnv, bind.onEvent, bind.onClose)
	if err != nil {
		if mcpCleanup != nil {
			mcpCleanup()
		}
		if req.Config.RequireResume && exclusiveHome != "" {
			return nil, fmt.Errorf("%w — exclusive CODEX_HOME %s", err, exclusiveHome)
		}
		return nil, err
	}
	sid := client.ThreadID()
	if exclusiveHome != "" && sid != "" {
		if err := publishExclusiveCodexHome(exclusiveHome, sid); err != nil {
			slog.Error("publish exclusive CODEX_HOME", "src", exclusiveHome, "session", sid, "err", err)
		}
		mcpCleanup = exclusiveCodexHomeCleanup(exclusiveHome, sid, mcpCleanup)
	}
	ops := agentOps{
		attachCommand: func(*Agent) string { return "" },
		interrupt: func(*Agent) error {
			return client.Interrupt()
		},
		send: func(_ *Agent, msg string) error {
			return client.Prompt(msg)
		},
		stop: func(*Agent) {
			client.Close()
		},
		promptInFlight: func(*Agent) bool {
			return client.promptInFlight()
		},
		setModel: func(_ *Agent, model string) error {
			return client.SetModel(model)
		},
		turnCaps: func(*Agent) TurnCaps {
			return codexTurnCaps(steerSupported)
		},
	}
	if steerSupported {
		ops.steer = steerOp(client)
	}
	return &agentStart{
		Cleanup:   mcpCleanup,
		WindowID:  "codex-app-server-" + sid,
		Ops:       ops,
		TailJSONL: false,
		SessionID: sid,
		DetectReady: func(a *Agent) {
			bind.attach(a)
			if m := client.Model(); m != "" {
				a.publishEvent(Event{Type: "system", Model: m})
			}
			close(a.ready)
		},
	}, nil
}

// startGrokAgent launches Grok Build over ACP. Default is parent-owned
// `grok agent stdio`. Connect-mode (Config.GrokConnect / CLAUDIA_GROK_CONNECT
// / ConnectURL) uses detached `grok agent serve` + WebSocket so the agent
// process survives consumer restart (jevons 🎯T40).
func startGrokAgent(req agentStartRequest) (*agentStart, error) {
	if err := grokSessionPrecheck(req); err != nil {
		return nil, err
	}

	bin, err := resolveGrokBin()
	if err != nil {
		return nil, err
	}

	plan := planGrokSession(req)
	preferID := plan.PreferSessionID

	// Client is closed via ops.stop. publishEvent is wired once Agent exists;
	// we stash a pointer-to-func that Start fills after construction by
	// closing ready immediately and using ops that capture the client.
	var bind acpBind

	var extraEnv []string
	var grokHome string
	if req.Config.MCPExclusive {
		home, herr := exclusiveGrokHomeForStart(preferID, plan.RequireResume || req.Config.ConnectURL != "")
		if herr != nil {
			return nil, herr
		}
		grokHome = home
		extraEnv = exclusiveEnv("GROK_HOME", home)
		slog.Info("grok MCPExclusive", "GROK_HOME", home)
	}

	var client *grokACPClient
	if plan.Connect {
		client, err = startGrokACPConnect(bin, plan.WorkDir, plan.Model, preferID, plan.RequireResume, plan.MCPServers, req.Config, extraEnv, bind.onEvent, bind.onClose)
	} else {
		client, err = startGrokACP(bin, plan.WorkDir, plan.Model, preferID, plan.RequireResume, plan.MCPServers, extraEnv, bind.onEvent, bind.onClose)
	}
	if err != nil {
		return nil, err
	}

	sid := client.SessionID()
	if grokHome != "" {
		if err := publishExclusiveGrokHome(grokHome, sid); err != nil {
			client.Close()
			return nil, err
		}
	}
	windowID := "grok-acp-" + sid
	if client.ConnectURL() != "" {
		windowID = "grok-serve-" + sid
	}
	ops := agentOps{
		attachCommand: func(*Agent) string {
			// Connect-mode: no TUI; surface endpoint for operators.
			if u := client.ConnectURL(); u != "" {
				return "grok-serve " + u
			}
			return ""
		},
		interrupt: func(*Agent) error {
			return client.Cancel()
		},
		send: func(_ *Agent, msg string) error {
			// Non-blocking: streams + terminal stop arrive via onEvent.
			return client.Prompt(msg)
		},
		resize: nil, // unsupported over ACP
		stop: func(*Agent) {
			client.Close()
		},
		promptInFlight: func(*Agent) bool {
			return client.promptInFlight()
		},
		setModel: func(_ *Agent, model string) error {
			return client.SetModel(model)
		},
		// Nil until grokACPClient satisfies turnSteerer (🎯T72.1); then
		// this same line wires Steer without an edit here.
		steer: steerOp(client),
	}

	return &agentStart{
		WindowID:   windowID,
		Ops:        ops,
		TailJSONL:  false,
		SessionID:  sid,
		ConnectURL: client.ConnectURL(),
		ConnectPID: client.ConnectPID(),
		// Leave JSONLPath empty: Grok Session is not a Claude JSONL transcript.
		DetectReady: func(a *Agent) {
			// Must run before Send: wire publishEvent target, then mark ready.
			bind.attach(a)
			select {
			case <-a.ready:
			default:
				close(a.ready)
			}
		},
	}, nil
}

// Run sends a single prompt to a new Session-mode agent, waits for
// completion, and returns the assistant's response text.
//
// WaitForResponse is started before Send so event subscription is active
// before the provider begins streaming (important for fast ACP fakes and
// quick local models).
func Run(ctx context.Context, prompt string, cfg Config) (string, error) {
	agent, err := StartContext(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer agent.Stop()

	type outcome struct {
		text string
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		text, err := agent.WaitForResponse(ctx)
		ch <- outcome{text, err}
	}()
	// Let WaitForResponse register its subscriber before Send.
	runtime.Gosched()

	if err := agent.Send(prompt); err != nil {
		return "", fmt.Errorf("send prompt: %w", err)
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-ch:
		return out.text, out.err
	}
}

// SessionID returns the Claude Code session ID.
func (a *Agent) SessionID() string { return a.sessionID }

// WindowID returns the tmux window id (e.g. "@3") for a Claude
// session, or "" when this agent is not tmux-backed.
func (a *Agent) WindowID() string { return a.tmuxWindowID }

// JSONLPath returns the path to the session JSONL file.
func (a *Agent) JSONLPath() string { return a.jsonlPath }

// SessionJSONLPath returns the path Claude Code would use for the
// given session ID and workdir, whether or not the file exists. The
// path is ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl, where
// the cwd encoding maps non-alphanumeric/dash runes to '-'. Callers
// embedding claudia (e.g. building a "resume meeting" flow) can use
// this together with [SessionExists] to decide between fresh-start
// and resume code paths before invoking [Start].
func SessionJSONLPath(sessionID, workDir string) string {
	return filepath.Join(projectDir(workDir), sessionID+".jsonl")
}

// SessionExists reports whether a Claude Code JSONL transcript exists
// on disk for the given session ID and workdir. It returns (false,
// nil) when the file is simply absent — only filesystem errors
// (permission denied, etc.) are propagated.
//
// Embedders should prefer this over reproducing the path computation
// themselves; the encoded-cwd convention is owned by Claude Code and
// claudia tracks it here.
func SessionExists(sessionID, workDir string) (bool, error) {
	_, err := os.Stat(SessionJSONLPath(sessionID, workDir))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// TermLogPath returns the path to the raw terminal output log, or ""
// if terminal logging is disabled or has been silently halted after a
// write error.
func (a *Agent) TermLogPath() string {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	if !a.termLogLive {
		return ""
	}
	return a.termLogPath
}

// AttachCommand returns the tmux incantation a human can paste to
// watch the live agent session. This is the primary observability
// mechanism for claudia agents.
func (a *Agent) AttachCommand() string {
	if a.ops.attachCommand == nil {
		return ""
	}
	return a.ops.attachCommand(a)
}

// Alive reports whether this consumer still holds a live control session
// (stdio child or WebSocket). For Grok connect-mode a dropped WebSocket
// makes Alive false even if the serve PID still runs — use [Agent.PID]
// + process reattach rather than treating the stale Agent as usable.
// Alive reports whether this agent can still be reached.
//
// For a tmux-backed session the WINDOW is the agent: if it is gone the
// agent is gone, whatever the in-process flag remembers. Returning the
// cached flag alone was a lie with consequences (🎯T602) — on 2026-08-31
// the overseer's window vanished and the daemon went on believing the
// process alive, so its converge loop chose "unstick" over "launch" and
// tried, every 90 seconds and forever, to send Escape to a window tmux
// had already forgotten:
//
//	cockpit: interrupt failed: tmux send-keys Escape: can't find window: @124
//
// It could not self-heal, because the one fact that would have triggered
// a relaunch was the fact it had wrong. Every stuck episode that day
// needed a human to restart the daemon.
//
// The window check is cached briefly: Alive is on the converge loop's
// path (every few seconds, per agent) and each miss costs a tmux exec.
func (a *Agent) Alive() bool {
	a.mu.Lock()
	alive := a.alive
	win := a.tmuxWindowID
	probe := a.windowAliveFn
	if !alive || win == "" || probe == nil {
		a.mu.Unlock()
		return alive
	}
	if time.Since(a.windowCheckAt) < windowCheckTTL {
		ok := a.windowCheckOK
		a.mu.Unlock()
		return ok
	}
	a.mu.Unlock()

	ok := probe(win)

	a.mu.Lock()
	a.windowCheckAt, a.windowCheckOK = time.Now(), ok
	if !ok {
		// Latch it: a window does not come back, and later callers
		// should not pay for the probe again.
		a.markDeadLocked()
	}
	a.mu.Unlock()
	return ok
}

// windowCheckTTL bounds how stale the tmux window answer may be. Short
// enough that a lost window is noticed within one converge tick, long
// enough that agent_list does not shell out per row per request.
const windowCheckTTL = 2 * time.Second

// PID returns the durable agent OS process id when known (Grok
// connect-mode serve PID). Zero for stdio children or Claude tmux
// (use AttachCommand / window tooling there).
func (a *Agent) PID() int {
	return a.connectPID
}

// ConnectURL returns the Grok connect-mode WebSocket URL for reattach, or "".
func (a *Agent) ConnectURL() string {
	return a.connectURL
}

// ProcessAlive reports whether the durable OS process is still running
// (connect-mode serve PID). Falls back to [Agent.Alive] when no PID is known.
func (a *Agent) ProcessAlive() bool {
	if a.connectPID > 0 {
		return processAlive(a.connectPID)
	}
	return a.Alive()
}

// SubscribeEvents registers fn to receive JSONL events and returns a
// subscription token. Pass the token to [Agent.UnsubscribeEvents] when done.
// Multiple subscribers are called in unspecified order on every event.
// The subscriber map is created on first use so a zero Agent is usable for
// hermetic fan-out tests (no Start required).
func (a *Agent) SubscribeEvents(fn EventFunc) int64 {
	id := nextEventSubID.Add(1)
	a.mu.Lock()
	if a.eventSubs == nil {
		a.eventSubs = make(map[int64]EventFunc)
	}
	a.eventSubs[id] = fn
	hook := a.onSubscribe
	a.onSubscribe = nil
	a.mu.Unlock()
	if hook != nil {
		hook()
	}
	return id
}

// UnsubscribeEvents removes the event subscriber identified by token.
func (a *Agent) UnsubscribeEvents(token int64) {
	a.mu.Lock()
	if a.eventSubs != nil {
		delete(a.eventSubs, token)
	}
	a.mu.Unlock()
}

// PublishEvent delivers ev to every current subscriber. Used by provider
// backends and hermetic tests that need to drive the fan-out without a
// live JSONL tail (e.g. jevons 🎯T210 double-attach oracle).
func (a *Agent) PublishEvent(ev Event) {
	a.publishEvent(ev)
}

// Interrupt sends the Escape key to the Claude process to cancel
// the current turn.
func (a *Agent) Interrupt() error {
	a.closeGoal()
	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("claude process not running")
	}
	if a.ops.interrupt == nil {
		return unsupportedCapability(a.provider, "interrupt", "provider did not supply an interrupt operation")
	}
	return a.ops.interrupt(a)
}

// PromptInFlight reports whether the provider has an open prompt/turn
// that blocks concurrent Send (Grok ACP promptID). False when unknown
// or unsupported. Used by jevons cockpit stuck-busy recovery (🎯T204).
func (a *Agent) PromptInFlight() bool {
	if a == nil || a.ops.promptInFlight == nil {
		return false
	}
	return a.ops.promptInFlight(a)
}

// Send writes a user message to the Claude process and submits it.
//
// Send blocks until Claude Code's TUI has finished initialising and
// is ready to accept input (see [Agent.WaitReady] for the detection
// strategy). This prevents keystrokes from being typed into a
// half-painted startup UI where they would be silently dropped.
// Once the session has reached readiness, subsequent Send calls
// return immediately — the ready channel stays closed for the life
// of the agent.
//
// Delivery: short single-line messages are typed via tmux send-keys -l;
// multi-line or large messages use load-buffer + bracketed paste so
// Claude Code receives one paste instead of a keystroke flood.
// Submit is a named Enter key; if a collapsed paste chip remains,
// additional Enters are pressed until the turn begins (composer leaves
// idle). Silent "keys sent, turn never started" is an error (🎯T305).
//
// If readiness detection failed (process exited during startup, or
// the overall timeout elapsed) Send returns the detection error
// without writing anything.
//
// Send is [Agent.SendMode] with [DeliverySubmit]; the steer and
// interrupt intents live there and on [Agent.Steer] (🎯T72.2).
func (a *Agent) Send(msg string) error {
	if err := a.deliverable(); err != nil {
		return err
	}
	if a.ops.send == nil {
		return unsupportedCapability(a.provider, "send", "provider did not supply a send operation")
	}
	// Before the write: a Cursor opening prompt does not return until the
	// peer has spoken, so the reply and its end_turn can be published
	// while ops.send is still unwinding (🎯T98).
	a.beginTurn()
	if err := a.ops.send(a, msg); err != nil {
		return err
	}
	a.recordInert(inertTurn{Role: "user", Text: strings.TrimSpace(msg)})
	return nil
}

// Provider returns the live Session provider (empty means Claude).
func (a *Agent) Provider() Provider {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.provider
}

// WaitReady blocks until the TUI has finished initialising and is
// ready to accept input from Send, or until ctx is cancelled. It
// returns any error recorded during readiness detection (e.g. Claude
// exited during startup, or the overall timeout elapsed).
//
// Calling this is optional: Send calls it internally on every
// invocation, so consumers that just want to send a prompt do not
// need to wait explicitly. WaitReady is exposed for consumers that
// want to observe the ready transition (e.g. to update a UI) or
// distinguish "readiness failed" from "send failed".
func (a *Agent) WaitReady(ctx context.Context) error {
	select {
	case <-a.ready:
		return a.readyErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Usage returns the cumulative token usage parsed from the JSONL transcript
// since the agent was started.
func (a *Agent) Usage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

// Model returns the model the backend resolved for this session, taken from
// the most recent event that carried a model id (Claude: assistant
// message.model; Codex app-server: thread/start result.model), e.g.
// "claude-opus-5". It is empty until the first such event. Compare it against
// [Config.Model] to detect silent fallback: an alias such as "opus" resolves
// to its full id here. An unusable model surfaces as "<synthetic>" with
// [Event.IsError] set — [Agent.WaitForResponse] returns that as a descriptive
// error rather than a normal reply. After a successful [Agent.SetModel], this
// reflects the requested model until a later event publishes a resolved id.
func (a *Agent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// SetModel switches the model used for subsequent turns on this Session
// without changing provider (🎯T54). Empty model is rejected. Refuses while
// a turn is in flight. Support is gated by [CapabilityModelSwitch]:
// Claude sends `/model <name>` into the TUI; Codex applies model on the
// next turn/start; Grok/Cursor use ACP session/set_config_option (with
// legacy session/set_model fallback). Task-only providers refuse.
//
// On success, [Agent.Model] is updated immediately to the requested string
// and a Type=system Event with that Model is published so subscribers see
// the switch; a later assistant event may refine Model to a fully resolved id.
func (a *Agent) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return fmt.Errorf("SetModel: model must be non-empty")
	}
	if err := CheckCapability(a.provider, CapabilityModelSwitch); err != nil {
		return err
	}
	<-a.ready
	if a.readyErr != nil {
		return fmt.Errorf("agent not ready: %w", a.readyErr)
	}
	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("agent process not running")
	}
	if a.PromptInFlight() {
		return fmt.Errorf("SetModel: turn in flight; wait for the current response or Interrupt first")
	}
	if a.ops.setModel == nil {
		return unsupportedCapability(a.provider, CapabilityModelSwitch,
			"provider did not supply a setModel operation")
	}
	if err := a.ops.setModel(a, model); err != nil {
		return err
	}
	if a.brokerGrant != "" {
		// The daemon's Agent published the switch event; it arrives on
		// the stream. Publishing again here would double it.
		a.mu.Lock()
		a.model = model
		a.mu.Unlock()
		return nil
	}
	a.publishEvent(Event{
		Type:      "system",
		SessionID: a.sessionID,
		Model:     model,
		Text:      "model switch: " + model,
	})
	return nil
}

func (a *Agent) publishEvent(ev Event) {
	a.mu.Lock()
	if ev.Model != "" {
		a.model = ev.Model
	}
	if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 ||
		ev.Usage.CacheCreationInputTokens > 0 || ev.Usage.CacheReadInputTokens > 0 {
		a.usage.InputTokens += ev.Usage.InputTokens
		a.usage.OutputTokens += ev.Usage.OutputTokens
		a.usage.CacheCreationInputTokens += ev.Usage.CacheCreationInputTokens
		a.usage.CacheReadInputTokens += ev.Usage.CacheReadInputTokens
	}
	if t, ok := noteInertFromEvent(ev); ok {
		a.recordInertLocked(t)
	}
	a.noteGoalEvent(ev)
	a.recordTurnEventLocked(ev)
	subs := make([]EventFunc, 0, len(a.eventSubs))
	for _, fn := range a.eventSubs {
		subs = append(subs, fn)
	}
	a.mu.Unlock()

	for _, fn := range subs {
		fn(ev)
	}

	if class, detail := classifyStuckEvent(ev); class != "" {
		// The provider just said the plan is exhausted, so every usage
		// snapshot on the host is wrong until re-read. A broker handle
		// leaves that to the daemon, whose own agent saw the same event.
		a.mu.Lock()
		brokered := a.brokerGrant != ""
		a.mu.Unlock()
		if !brokered {
			invalidatePlanUsage()
		}
		stuck := Event{
			Type:         "system",
			ProgressType: ProgressStuck,
			SessionID:    a.SessionID(),
			StuckClass:   class,
			Text:         detail,
			IsError:      true,
		}
		a.mu.Lock()
		a.recordTurnEventLocked(stuck)
		stuckSubs := make([]EventFunc, 0, len(a.eventSubs))
		for _, fn := range a.eventSubs {
			stuckSubs = append(stuckSubs, fn)
		}
		a.mu.Unlock()
		for _, fn := range stuckSubs {
			fn(stuck)
		}
	}
}

// frameDrops reports the broker frames this seat's connection has skipped as
// too large to relay; zero for a seat not reached over a broker.
func (a *Agent) frameDrops() frameDrops {
	if a.ops.droppedFrames == nil {
		return frameDrops{}
	}
	n, last := a.ops.droppedFrames(a)
	return frameDrops{n: n, last: last}
}

// rewindOnBroker asks the daemon holding this seat to rewind it.
func (a *Agent) rewindOnBroker(n int) (*RewindResult, error) {
	if a.ops.rewind == nil {
		return nil, fmt.Errorf("Rewind: seat %s handle cannot reach the daemon's rewind", a.brokerGrant)
	}
	return a.ops.rewind(a, n)
}

// EventSubscriberCount returns how many live event subscribers are registered.
// Hermetic oracle for fan-out idempotency (e.g. single chat attach after re-attach).
func (a *Agent) EventSubscriberCount() int {
	a.mu.Lock()
	n := len(a.eventSubs)
	a.mu.Unlock()
	return n
}

// WaitForResponse blocks until the next assistant turn completes and
// returns the assistant text accumulated across the turn.
//
// A single logical assistant message can be split across multiple
// JSONL events — one per content block (thinking, text, tool_use,
// etc.). In some Claude Code versions every block in a message
// carries the message's stop_reason, not just the last one; in
// others only the final block does. WaitForResponse therefore does
// not resolve on the first terminal stop_reason it sees. Instead,
// it starts a short settle timer when a terminal event arrives and
// resets the timer on every subsequent assistant event. The
// accumulated text is returned only once the timer expires without
// new events — the heuristic for "all content blocks of this turn
// have arrived". The settle delay trades a small constant latency
// (waitSettleDuration) against the risk of emitting an incomplete
// message.
//
// Completion stop reasons are end_turn, stop_sequence, and
// max_tokens. A tool_use stop reason is not terminal: the model
// paused for tool results and will emit further assistant events
// as the turn continues, and those events will keep the settle
// timer from firing.
//
// Fail-loud (🎯T16): when an event has [Event.IsError] (e.g. Claude
// model_not_found with model "<synthetic>", or a Codex failed turn),
// WaitForResponse returns a descriptive error immediately — never
// treating the failure text as a normal reply and never hanging until
// the caller's context times out.
func (a *Agent) WaitForResponse(ctx context.Context) (string, error) {
	type outcome struct {
		text string
		err  error
	}
	ch := make(chan outcome, 1)
	var (
		mu           sync.Mutex
		text         strings.Builder
		seenTerminal bool
		settleTimer  *time.Timer
		emitted      bool
		witness      turnWitness
	)

	// The wake set (🎯T96). ctx and the turn's own terminal event are the
	// two the caller can see. The rest are here because a turn that never
	// ends must still end the wait: this select used to have no other
	// arm, so a consumer passing a long-lived context — daemon code, or a
	// test's t.Context() — waited for the life of the process.
	started := a.now()
	dropsAtStart := a.frameDrops()
	// The bound answers to the deadline this wait is actually running
	// under (🎯T103): under `go test` that is the test binary's own
	// timeout, and a thirty-minute bound inside a ten-minute binary is a
	// diagnosis nobody ever hears. In production there is no such
	// deadline and the configured bound is untouched.
	bound := a.waitBound(started)

	// A wait that BEGAN on a live agent is woken by that agent's death: a
	// dead agent provably cannot publish the terminal event, so there is
	// nothing left to wait for. A wait that began on an agent already
	// dead (or on a bare hermetic fixture, which is never alive) keeps
	// the old behaviour and leans on the silence bound.
	var deadCh <-chan struct{}
	var liveTick <-chan time.Time
	if a.Alive() {
		deadCh = a.deadSignal()
		// A killed tmux window closes nothing and ends no stream, so the
		// only thing that knows is the probe, and only when asked.
		a.mu.Lock()
		probing := a.windowAliveFn != nil && a.tmuxWindowID != ""
		a.mu.Unlock()
		if probing {
			liveTick = a.after(turnLivenessPollInterval)
		}
	}
	// Armed before the subscription, not after: a wake the wait installs
	// only once it is already listening is a wake no test can prove is
	// there, and this one exists precisely because its absence is
	// invisible until a suite hangs.
	silence := a.after(bound.effective)

	emitOnce := func(out outcome) {
		mu.Lock()
		if emitted {
			mu.Unlock()
			return
		}
		emitted = true
		if settleTimer != nil {
			settleTimer.Stop()
		}
		mu.Unlock()
		select {
		case ch <- out:
		default:
		}
	}

	emitOK := func() {
		mu.Lock()
		result := text.String()
		if emitted {
			mu.Unlock()
			return
		}
		emitted = true
		mu.Unlock()
		select {
		case ch <- outcome{text: result}:
		default:
		}
	}

	onEvent := func(ev Event) {
		// Every event, of every type, is turn activity: the silence
		// bound below asks whether the agent is saying ANYTHING, not
		// whether it has answered yet.
		witness.note(ev, a.now())
		if ev.IsError {
			msg := strings.TrimSpace(ev.Text)
			if msg == "" {
				msg = "agent turn failed"
			}
			emitOnce(outcome{err: errors.New(msg)})
			return
		}
		if ev.Type != "assistant" {
			return
		}

		mu.Lock()
		appendTurnText(&text, ev)
		if ev.IsTerminalStop() {
			seenTerminal = true
		}
		armed := seenTerminal
		if armed {
			if settleTimer != nil {
				settleTimer.Stop()
			}
			settleTimer = time.AfterFunc(waitSettleDuration, emitOK)
		}
		mu.Unlock()
	}

	// A turn that ended before this call subscribed is handed over here,
	// in the same critical section that registers onEvent: a subscription
	// only hears the future, and a blocking submit can consume the whole
	// turn before its caller ever reaches this line (🎯T98).
	token := a.subscribeEventsTakingTurn(onEvent, func(prior turnSoFar) {
		if prior.err != nil {
			emitOnce(outcome{err: prior.err})
			return
		}
		mu.Lock()
		text.WriteString(prior.text)
		seenTerminal = prior.terminal
		if seenTerminal {
			settleTimer = time.AfterFunc(waitSettleDuration, emitOK)
		}
		mu.Unlock()
	})

	defer a.UnsubscribeEvents(token)
	defer func() {
		mu.Lock()
		if settleTimer != nil {
			settleTimer.Stop()
		}
		mu.Unlock()
	}()

	// lastActivity is the most recent sign of life from the agent, from
	// any source: a published event, or a terminal byte (a working TUI
	// repaints while a tool runs, when the transcript says nothing).
	lastActivity := func() time.Time {
		last := started
		if t := witness.lastEventAt(); t.After(last) {
			last = t
		}
		if t := a.lastTermActivity(); t.After(last) {
			last = t
		}
		return last
	}
	fail := func(cause error, now, last time.Time) (string, error) {
		snap := witness.snapshot()
		mu.Lock()
		snap.chars = text.Len()
		mu.Unlock()
		return "", a.turnWaitError(cause, snap, now, started, last, bound, a.frameDrops().since(dropsAtStart))
	}
	// answered drains a result that landed in the same instant as one of
	// the failure wakes. An agent that died right after saying its piece
	// said its piece.
	answered := func() (outcome, bool) {
		select {
		case out := <-ch:
			return out, true
		default:
			return outcome{}, false
		}
	}

	for {
		select {
		case <-ctx.Done():
			if out, ok := answered(); ok {
				// The turn landed in the same instant the caller's
				// context ended. An agent that said its piece said it.
				return out.text, out.err
			}
			// The caller's own deadline is the other deadline a wait runs
			// under, and a bare context.DeadlineExceeded names nothing:
			// not the session, not the turn, not what last arrived
			// (🎯T103). The cause is still wrapped, so errors.Is keeps
			// working for callers that test for it.
			return fail(ctx.Err(), a.now(), lastActivity())

		case out := <-ch:
			return out.text, out.err

		case <-deadCh:
			if out, ok := answered(); ok {
				return out.text, out.err
			}
			if witness.sawTerminal() {
				// The turn DID end; only the settle timer was still
				// running, and nothing more can arrive to extend it.
				// Settle now rather than discard a complete answer.
				emitOK()
				out := <-ch
				return out.text, out.err
			}
			return fail(ErrAgentGone, a.now(), lastActivity())

		case <-liveTick:
			// Alive latches a lost window and closes deadCh, so the death
			// is handled in one place: the arm above, on the next pass.
			// A turn that is producing needs no probe — it is answering
			// the liveness question itself, for free — and skipping it
			// there keeps a busy fleet from paying a tmux exec per seat
			// per interval for an answer it already has.
			if a.now().Sub(lastActivity()) >= turnLivenessPollInterval {
				a.Alive()
			}
			liveTick = a.after(turnLivenessPollInterval)

		case <-silence:
			if out, ok := answered(); ok {
				return out.text, out.err
			}
			select {
			case <-deadCh:
				// The agent died and the bound expired in the same
				// breath. Death is the more specific answer, and the
				// true one: this turn is not merely quiet.
				return fail(ErrAgentGone, a.now(), lastActivity())
			default:
			}
			now, last := a.now(), lastActivity()
			if idle := now.Sub(last); idle < bound.effective {
				// Something arrived while the timer ran: this is a bound
				// on SILENCE, so the clock restarts from that activity,
				// not from the wait.
				silence = a.after(bound.effective - idle)
				continue
			}
			return fail(ErrTurnAbandoned, now, last)
		}
	}
}

// waitSettleDuration is how long WaitForResponse lingers after
// seeing a terminal stop_reason to let any remaining content blocks
// of the same message arrive. 250ms is comfortably longer than the
// ~45ms gap observed between thinking and text blocks of a single
// message in Claude Code v2.1.101, while still short enough to be
// imperceptible to most consumers.
const waitSettleDuration = 250 * time.Millisecond

// Resize changes the terminal dimensions of the tmux window. Useful when
// embedding terminal output in a variable-size UI.
func (a *Agent) Resize(cols, rows uint16) error {
	if a.ops.resize == nil {
		return unsupportedCapability(a.provider, "resize", "provider did not supply a resize operation")
	}
	return a.ops.resize(a, cols, rows)
}

// Stop kills the tmux window, closes the control-mode connection, and
// flushes the terminal log. It is idempotent. For agents obtained via
// [Acquire], call [Agent.Release] instead — Stop does not return the
// window to the pool.
func (a *Agent) Stop() {
	a.stopOnce.Do(func() {
		a.closeGoal()
		if a.ops.stop != nil {
			a.ops.stop(a)
		}
		if a.mcpCleanup != nil {
			a.mcpCleanup()
			a.mcpCleanup = nil
		}

		a.termMu.Lock()
		if a.termLog != nil {
			a.termLog.Close()
			a.termLog = nil
			a.termLogLive = false
		}
		a.termMu.Unlock()
	})
}

// Rewind rolls this Session-mode agent back by n user turns and resumes the
// session, returning a fresh [Agent] positioned at the rewound state. The
// receiver is stopped: rewinding must kill the live claude process (which holds
// the conversation in memory and would otherwise re-append the dropped turns),
// truncate the transcript, then start a new process with --resume — so all
// derived state (token usage, the JSONL tailer, TUI readiness) is rebuilt by the
// resume rather than patched in place.
//
// cfg supplies the resume parameters (WorkDir, Model, MCPConfig, …) and should
// match the config the agent was started with; its SessionID is overridden with
// this agent's session id. Turn-boundary and undo semantics are those of
// [RewindSession]: tool-result entries are not counted as turns, so a rewind
// never lands mid-tool-use, and the pre-rewind transcript is backed up.
//
// On a seat held by a claudia daemon the daemon rewinds and relaunches it,
// and the returned Agent is the receiver itself, re-pointed at the relaunched
// process with its subscriptions kept: the grant never lapses. For a seat a
// [Registry] launched, use [Registry.Rewind], which keeps the Registry's
// handle current.
func (a *Agent) Rewind(n int, cfg Config) (*Agent, error) {
	// Either provider naming a non-Claude runtime is enough to refuse:
	// rewinding means truncating a Claude-shaped JSONL transcript, which
	// is meaningless — and, for providers whose state is private,
	// forbidden — anywhere else.
	for _, provider := range []Provider{a.provider, cfg.Provider} {
		if err := CheckCapability(provider, CapabilityRewind); err != nil {
			return nil, err
		}
	}
	if a.brokerGrant != "" {
		if _, err := a.rewindOnBroker(n); err != nil {
			return nil, err
		}
		return a, nil
	}
	a.Stop()
	if _, err := rewindJSONL(a.jsonlPath, n); err != nil {
		return nil, err
	}
	cfg.SessionID = a.sessionID
	return Start(cfg)
}

// detectReady polls capture-pane for Claude Code's idle input box
// at the bottom of the rendered viewport. The empty-prompt-box regex
// matches a horizontal rule, the ❯ prompt glyph, another horizontal
// rule, and up to 5 trailing status lines.
//
// This replaces the earlier PTY-silence heuristic. The rendered-frame
// approach is cleaner: it detects a fixed-point visual state rather
// than inferring readiness from the absence of byte traffic.
func (a *Agent) detectReady() {
	defer close(a.ready)

	_, err := tmuxagent.WaitReady(a.tmuxWindowID, readyPollInterval, readyOverallTimeout)
	if err != nil {
		a.readyErr = err
	}
}

const termBufSize = 128 * 1024 // 128KB ring buffer

func (a *Agent) pushTermOutput(data []byte) {
	a.termMu.Lock()
	defer a.termMu.Unlock()

	a.termActivityAt = a.now()
	a.termBuf = append(a.termBuf, data...)
	if len(a.termBuf) > termBufSize {
		a.termBuf = a.termBuf[len(a.termBuf)-termBufSize:]
	}

	if a.termLog != nil {
		if _, err := a.termLog.Write(data); err != nil {
			slog.Warn("term log write failed", "path", a.termLogPath, "err", err)
			a.termLog.Close()
			a.termLog = nil
			a.termLogLive = false
		}
	}

	for _, ch := range a.termSubs {
		select {
		case ch <- data:
		default:
		}
	}
}

// SubscribeTerminal returns a channel that receives live terminal
// output and the buffered recent output. Call
// [Agent.UnsubscribeTerminal] when done.
func (a *Agent) SubscribeTerminal() (history []byte, ch chan []byte) {
	if a.ops.subscribeTerminal != nil {
		a.termMu.Lock()
		first := !a.termSubscribed
		a.termSubscribed = true
		a.termMu.Unlock()
		if first {
			a.ops.subscribeTerminal(a)
		}
	}
	a.termMu.Lock()
	defer a.termMu.Unlock()

	ch = make(chan []byte, 256)
	a.termSubs = append(a.termSubs, ch)

	history = make([]byte, len(a.termBuf))
	copy(history, a.termBuf)
	return
}

// UnsubscribeTerminal removes a terminal subscriber.
func (a *Agent) UnsubscribeTerminal(ch chan []byte) {
	a.termMu.Lock()
	defer a.termMu.Unlock()

	for i, c := range a.termSubs {
		if c == ch {
			a.termSubs = append(a.termSubs[:i], a.termSubs[i+1:]...)
			close(ch)
			return
		}
	}
}

func (a *Agent) tailJSONL() { a.tailJSONLFrom(0) }

// tailStartOffset is where a tailer that wants to resume at want should
// actually begin, given the transcript is size bytes long. An offset past
// the end means the file was rewritten under us and the bytes there now
// are not the bytes we measured, so the end is the honest starting point:
// replaying them would publish a conversation this holder never had.
func tailStartOffset(size, want int64) int64 {
	if want > size {
		return size
	}
	return want
}

// tailJSONLFrom is tailJSONL starting at a byte offset into the
// transcript. A fresh Start owns its transcript from byte zero, so it
// tails from 0; a Start that resumes one tails from where the resumed
// conversation ends (🎯T114). A pooled window's transcript already holds the turns of
// every holder before this one, and offset is the end of the file at the
// moment this holder acquired it: replaying those turns into the new
// holder's subscribers would be somebody else's conversation arriving as
// if it were this one's (🎯T78).
func (a *Agent) tailJSONLFrom(offset int64) {
	gen := a.backendGen.Load()
	// Wait for file to be created.
	for {
		if a.backendGen.Load() != gen {
			return
		}
		if _, err := os.Stat(a.jsonlPath); err == nil {
			break
		}
		if !a.Alive() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	f, err := os.Open(a.jsonlPath)
	if err != nil {
		slog.Error("open JSONL failed", "session", a.sessionID, "err", err)
		return
	}
	defer f.Close()

	if offset > 0 {
		if fi, statErr := f.Stat(); statErr == nil {
			offset = tailStartOffset(fi.Size(), offset)
		}
		if _, seekErr := f.Seek(offset, io.SeekStart); seekErr != nil {
			slog.Error("seek JSONL failed", "session", a.sessionID, "offset", offset, "err", seekErr)
			return
		}
	}

	reader := bufio.NewReader(f)
	var correlator claudeEventCorrelator
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if a.backendGen.Load() != gen || !a.Alive() {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		ev := correlator.correlate(parseEvent(line))
		a.applyClaudeTUIPreviewTranscript(ev)
		a.publishEvent(ev)
	}
}

const tuiPreviewPollInterval = 100 * time.Millisecond

// pollTUIPreview periodically captures the Claude tmux pane and publishes
// provisional ⏺ preview (and invariant fault) Events (🎯T51 / 🎯T51.4).
func (a *Agent) pollTUIPreview() {
	gen := a.backendGen.Load()
	ticker := time.NewTicker(tuiPreviewPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if a.backendGen.Load() != gen || !a.Alive() {
				return
			}
			if a.capturePane == nil {
				continue
			}
			frame, err := a.capturePane()
			if err != nil {
				continue
			}
			a.ObservePaneFrame(frame)
		}
	}
}

// ObservePaneFrame feeds a capture-pane frame into the Claude TUI preview
// tracker and publishes resulting Events. Hermetic tests inject fixtures
// here; the live path uses pollTUIPreview (🎯T51).
func (a *Agent) ObservePaneFrame(frame string) {
	a.tuiPreviewMu.Lock()
	tr := a.tuiPreview
	if tr == nil {
		a.tuiPreviewMu.Unlock()
		return
	}
	evs := tr.observeFrame(frame)
	a.tuiPreviewMu.Unlock()

	for _, ev := range evs {
		ev.SessionID = a.sessionID
		if ev.ProgressType == ProgressTUIPreviewFault {
			slog.Error("claude tui preview invariant failed",
				"session", a.sessionID,
				"turn", ev.TurnID,
				"message_id", ev.MessageID,
				"report", ev.Text,
			)
		}
		a.publishEvent(ev)
	}
}

// applyClaudeTUIPreviewTranscript updates turn baseline / seal state from
// transcript events. User prompts open a turn; assistant-text seals the
// next open ⏺ in order (🎯T51.3).
func (a *Agent) applyClaudeTUIPreviewTranscript(ev Event) {
	a.tuiPreviewMu.Lock()
	defer a.tuiPreviewMu.Unlock()
	if a.tuiPreview == nil {
		return
	}
	if len(ev.Raw) > 0 && isUserPromptLine(ev.Raw) {
		turnID := ev.RecordID
		if turnID == "" {
			turnID = ev.TurnID
		}
		a.tuiPreview.resetTurn(turnID)
		return
	}
	if ev.Type == "assistant" && strings.TrimSpace(ev.Text) != "" {
		a.tuiPreview.sealAssistantText()
	}
}

// projectDir returns the Claude Code project directory for a workdir.
func projectDir(workDir string) string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "projects", escapeWorkDir(workDir))
}

// termLogDir returns the directory under which raw terminal output
// logs are written for a given workdir. Follows XDG_STATE_HOME, with
// a ~/.local/state fallback.
func termLogDir(workDir string) string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(os.Getenv("HOME"), ".local", "state")
	}
	return filepath.Join(stateHome, "claudia", "terms", escapeWorkDir(workDir))
}

// escapeWorkDir applies Claude Code's workdir-escape scheme:
// non-alphanumeric/dash runes become '-'.
func escapeWorkDir(workDir string) string {
	var b strings.Builder
	for _, r := range workDir {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
