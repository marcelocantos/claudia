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
	Provider Provider

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

	// MCPConfig is the path to an MCP config JSON file.
	// Empty means Claude Code uses its default discovery.
	MCPConfig string

	// DisallowTools lists additional tool names to disallow. Agent,
	// TeamCreate, TeamDelete, SendMessage, and EnterWorktree are
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

	mu        sync.Mutex
	alive     bool
	stopOnce  sync.Once
	eventSubs map[int64]EventFunc
	usage     Usage
	model     string // resolved model from the latest event that carried one

	// poolWindow is true when the agent was acquired from the warm pool
	// (via Acquire) rather than spawned fresh (via Start). Pool agents
	// must be released via Release rather than stopped with Stop.
	poolWindow bool
	// poolWorkDir is the resolved absolute working directory used as
	// part of the pool key. Set by buildPoolAgent; empty for Start agents.
	poolWorkDir string

	// Terminal output streaming. termMu also guards termLog writes,
	// termLog close, and termLogLive so Stop cannot close the file
	// while pushTermOutput is mid-write.
	termMu      sync.Mutex
	termBuf     []byte
	termSubs    []chan []byte
	termLog     *os.File
	termLogLive bool // false once the log file has been closed or failed to open

	// TUI readiness. ready closes once detectReady concludes, either
	// because the capture-pane regex matched (success, readyErr == nil)
	// or because detection gave up (failure, readyErr set). Send
	// blocks on this channel before writing to the agent.
	ready    chan struct{}
	readyErr error
}

// nextEventSubID is a process-wide counter for event subscription tokens.
var nextEventSubID atomic.Int64

// Readiness detection tuning.
const (
	readyOverallTimeout = 30 * time.Second
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
	// promptInFlight is optional (Grok ACP). Nil → always false.
	promptInFlight func(*Agent) bool
}

type agentStartRequest struct {
	Config          Config
	WorkDir         string
	SessionID       string
	JSONLPath       string
	TermLogPath     string
	Resuming        bool
	DisallowedTools string
}

type agentStart struct {
	WindowID             string
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
}

type agentBackend interface {
	Capabilities() providerCapabilities
	StartAgent(agentStartRequest) (*agentStart, error)
}

type claudeAgentBackend struct{}

type codexAgentBackend struct{}

type grokAgentBackend struct{}

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

func (b errorAgentBackend) Capabilities() providerCapabilities {
	return providerCapabilities{}
}

func (b errorAgentBackend) StartAgent(agentStartRequest) (*agentStart, error) {
	return nil, b.err
}

func claudeAgentOps() agentOps {
	return agentOps{
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
	}
}

// Start spawns a new agent for cfg.Provider. Claude uses a tmux-backed
// Session; Grok uses ACP over `grok agent stdio`; Codex uses
// `codex app-server` JSON-RPC.
func Start(cfg Config) (*Agent, error) {
	// Gate on the published capability matrix rather than a per-provider
	// branch list, so Start cannot drift into offering a session claudia
	// has not claimed. Unknown providers still fall through to the
	// backend's own "unknown provider" error.
	if _, known := providerCapabilityClaims[cfg.Provider]; known || cfg.Provider == "" {
		if err := CheckCapability(cfg.Provider, CapabilitySession); err != nil {
			return nil, err
		}
	}
	return startConsideringBroker(cfg, agentBackendForProvider(cfg.Provider))
}

func startWithBackend(cfg Config, backend agentBackend) (*Agent, error) {
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
	if sessionID == "" {
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

	disallowed := disallowedToolList(cfg.DisallowTools)

	a := &Agent{
		provider:    provider,
		sessionID:   sessionID,
		jsonlPath:   jsonlPath,
		termLogPath: termLogPath,
		alive:       true,
		ready:       make(chan struct{}),
		eventSubs:   make(map[int64]EventFunc),
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
		Config:          cfg,
		WorkDir:         workDir,
		SessionID:       sessionID,
		JSONLPath:       jsonlPath,
		TermLogPath:     termLogPath,
		Resuming:        resuming,
		DisallowedTools: disallowed,
	})
	if err != nil {
		return nil, err
	}
	// A backend that returns neither a session nor an error is a bug in
	// that backend, but the cost of trusting it is a nil dereference
	// several lines below, well away from the cause.
	if start == nil {
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
	a.tmuxCtrl = start.Control
	a.ops = start.Ops
	a.connectURL = start.ConnectURL
	a.connectPID = start.ConnectPID

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
		go func() {
			for data := range start.Control.Bytes() {
				a.pushTermOutput(data)
			}
			slog.Debug("terminal control stream closed", "session", sessionID)
			a.mu.Lock()
			a.alive = false
			a.mu.Unlock()
		}()
	}

	if start.TailJSONL {
		go a.tailJSONL()
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
	args := []string{
		"--permission-mode", req.Config.PermissionMode,
		"--disallowedTools", req.DisallowedTools,
	}
	if req.Resuming {
		args = append(args, "--resume", req.SessionID)
	} else {
		args = append(args, "--session-id", req.SessionID)
	}
	if req.Config.MCPConfig != "" {
		args = append(args, "--mcp-config", req.Config.MCPConfig)
	}
	if req.Config.Model != "" {
		args = append(args, "--model", req.Config.Model)
	}
	return append(args, req.Config.ExtraArgs...)
}

func (claudeAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	if err := checkTmux(); err != nil {
		return nil, err
	}

	args := claudeAgentArgs(req)

	windowName := tmuxagent.SessionWindowName(req.SessionID)
	claudeBin, err := resolveClaudeBin()
	if err != nil {
		return nil, err
	}
	windowID, err := tmuxagent.SpawnWindow(req.WorkDir, windowName, claudeBin, args)
	if err != nil {
		return nil, fmt.Errorf("tmux spawn: %w", err)
	}

	start, err := attachClaudeWindow(windowID)
	if err != nil {
		tmuxagent.KillWindow(windowID)
		return nil, err
	}
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
}

func planGrokSession(req agentStartRequest) grokSessionPlan {
	// Prefer caller SessionID when resuming; empty means session/new.
	preferID := ""
	if req.Resuming || req.Config.SessionID != "" {
		preferID = req.SessionID
	}
	connect := grokConnectEnabled(req.Config)
	return grokSessionPlan{
		Args:            grokACPArgs(req.Config.Model, connect),
		WorkDir:         req.WorkDir,
		Model:           req.Config.Model,
		PreferSessionID: preferID,
		RequireResume:   req.Config.RequireResume,
		MCPServers:      acpMCPServers(req.Config.MCPConfig),
		Connect:         connect,
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
	return nil
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

	var agentRef atomic.Pointer[Agent]
	onEvent := func(ev Event) {
		if a := agentRef.Load(); a != nil {
			a.publishEvent(ev)
		}
	}
	onClose := func() {
		if a := agentRef.Load(); a != nil {
			a.mu.Lock()
			a.alive = false
			a.mu.Unlock()
		}
	}

	client, err := startCodexAppServer(bin, req.WorkDir, req.Config.Model, req.SessionID, req.Config.RequireResume, onEvent, onClose)
	if err != nil {
		return nil, err
	}
	sid := client.ThreadID()
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
	}
	return &agentStart{
		WindowID:  "codex-app-server-" + sid,
		Ops:       ops,
		TailJSONL: false,
		SessionID: sid,
		DetectReady: func(a *Agent) {
			agentRef.Store(a)
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
	var agentRef atomic.Pointer[Agent]
	onEvent := func(ev Event) {
		if a := agentRef.Load(); a != nil {
			a.publishEvent(ev)
		}
	}
	onClose := func() {
		if a := agentRef.Load(); a != nil {
			a.mu.Lock()
			a.alive = false
			a.mu.Unlock()
		}
	}

	var client *grokACPClient
	if plan.Connect {
		client, err = startGrokACPConnect(bin, plan.WorkDir, plan.Model, preferID, plan.RequireResume, plan.MCPServers, req.Config, onEvent, onClose)
	} else {
		client, err = startGrokACP(bin, plan.WorkDir, plan.Model, preferID, plan.RequireResume, plan.MCPServers, onEvent, onClose)
	}
	if err != nil {
		return nil, err
	}

	sid := client.SessionID()
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
			agentRef.Store(a)
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
	agent, err := Start(cfg)
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
func (a *Agent) Alive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.alive
}

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
	a.mu.Unlock()
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
func (a *Agent) Send(msg string) error {
	<-a.ready
	if a.readyErr != nil {
		return fmt.Errorf("claude not ready: %w", a.readyErr)
	}

	a.mu.Lock()
	alive := a.alive
	a.mu.Unlock()
	if !alive {
		return fmt.Errorf("claude process not running")
	}
	if a.ops.send == nil {
		return unsupportedCapability(a.provider, "send", "provider did not supply a send operation")
	}
	return a.ops.send(a, msg)
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
// error rather than a normal reply.
func (a *Agent) Model() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
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
	subs := make([]EventFunc, 0, len(a.eventSubs))
	for _, fn := range a.eventSubs {
		subs = append(subs, fn)
	}
	a.mu.Unlock()

	for _, fn := range subs {
		fn(ev)
	}
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
	)

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

	token := a.SubscribeEvents(func(ev Event) {
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
		if ev.Text != "" {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(ev.Text)
		}
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
	})

	defer a.UnsubscribeEvents(token)
	defer func() {
		mu.Lock()
		if settleTimer != nil {
			settleTimer.Stop()
		}
		mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-ch:
		return out.text, out.err
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
		if a.ops.stop != nil {
			a.ops.stop(a)
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

func (a *Agent) tailJSONL() {
	// Wait for file to be created.
	for {
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

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if !a.Alive() {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		ev := parseEvent(line)

		a.publishEvent(ev)
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
