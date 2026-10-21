// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

// ErrSeatIdentityMismatch is the sidecar refusing a seat because a seat of
// that name is already loaded under another session (server.ts checks the
// session, not the provider or purpose). A fresh start replaces such a seat;
// a start that requires its conversation, and an adopt, get this (jevons 🎯T1096).
var ErrSeatIdentityMismatch = errors.New("omp: seat is loaded in the sidecar under another session")
var ErrNoSubscriptionModel = errors.New("omp: no provider-local subscription session model")

// ErrOMPCodexSandboxUnsupported refuses a Codex sandbox or approval setting
// the sidecar cannot honour (anything but approval "never", or an unknown
// sandbox mode). Supported settings map onto the sidecar's tools (🎯T178).
var ErrOMPCodexSandboxUnsupported = errors.New("omp: codex sandbox or approval setting unsupported")

// ErrOMPSandboxNotHonoured refuses a seat whose sidecar did not confirm the
// requested tool policy, rather than run it with more access than asked.
var ErrOMPSandboxNotHonoured = errors.New("omp: sidecar did not apply the requested sandbox")

// ErrOMPMissingTurnID refuses sidecar answer payloads that cannot be tied to a turn.
var ErrOMPMissingTurnID = errors.New("omp: answer payload missing turn_id")

// ErrContextOverflow is what [Agent.WaitForResponse] wraps when the provider
// refused the turn as longer than the model's context window and the seat's
// own compaction could not bring it back under (🎯T148). It is terminal:
// sending the seat the same or another prompt cannot succeed, so a caller
// must not retry it as it would a transient failure. Jevons retried one
// every 30s for hours, each retry growing the prompt (jevons 🎯T926).
var ErrContextOverflow = errors.New("context overflow: the conversation is longer than the model's window")

// ReasonContextOverflow is [Event.Reason] on the error event of such a turn.
const ReasonContextOverflow = "context_overflow"

// ReasonAborted is [Event.Reason] on the final event of a turn the host
// aborted. Such a turn has no reply, so its Text is empty; it still ends
// with StopReason "end_turn", so every waiter's notion of a finished turn is
// unchanged. How it ended is said here, never as text a transcript would
// paint as an answer.
const ReasonAborted = "aborted"

// useOMP selects the sidecar. The four subscription plans and the
// fleet ids grok / claude / codex / cursor all go through it (🎯T866.5).
// Config.OMP is no longer required for cursor.

func useOMP(cfg Config) bool {
	return ompProviderID(cfg.Provider) != ""
}

func ompProviderID(p Provider) string {
	switch p {
	case "", Provider(omp.Anthropic), ProviderClaude:
		return omp.Anthropic
	case Provider(omp.OpenAICodex), ProviderCodex:
		return omp.OpenAICodex
	case ProviderCursor:
		return omp.Cursor
	case Provider(omp.XAIOAuth), ProviderGrok:
		return omp.XAIOAuth
	default:
		return ""
	}
}

func agentBackendFor(cfg Config) agentBackend {
	if useOMP(cfg) {
		return ompAgentBackend{}
	}
	return agentBackendForProvider(cfg.Provider)
}

// ompSandboxFor maps a sidecar seat's sandbox settings onto the sidecar's
// tool policy (🎯T178):
//
//	workspace-write  Write and Bash, writes confined to the workdir (+ roots)
//	SandboxGitWrite  adds the repo's git dirs; without it they stay protected
//	read-only        no Write, no Bash
//	approval         "never" (the default) only; anything else is refused
//
// Bare codex with no mode keeps the safe read-only default (🎯T37). A
// subscription-id seat with no mode, and danger-full-access, get no policy:
// the tools as they always were.

func ompProtectedPathRules(cfg Config) []omp.ProtectedPathRule {
	if len(cfg.ProtectedPaths) == 0 {
		return nil
	}
	out := make([]omp.ProtectedPathRule, len(cfg.ProtectedPaths))
	for i, r := range cfg.ProtectedPaths {
		out[i] = omp.ProtectedPathRule{Pattern: r.Pattern, Reason: r.Reason}
	}
	return out
}

func ompSandboxFor(cfg Config) (*omp.Sandbox, error) {
	if ompProviderID(cfg.Provider) == "" {
		// Not a sidecar seat: nothing here can enforce the policy.
		if sandboxPolicyRequested(cfg) {
			return nil, capabilityRefusal(cfg.Provider, CapabilitySandboxPolicy, sandboxPolicyIsCodexOnlyReason)
		}
		return nil, nil
	}
	// The sidecar's tools are provider-agnostic, so every sidecar seat
	// (anthropic, cursor, grok, codex) gets the same mapping. A claude seat
	// whose grant carries sandbox_policy resumes confined instead of being
	// refused (ge-po, regression from 🎯T178).
	describe := func() error {
		return fmt.Errorf("%w: SandboxMode=%q ApprovalPolicy=%q SandboxGitWrite=%t",
			ErrOMPCodexSandboxUnsupported, cfg.SandboxMode, cfg.ApprovalPolicy, cfg.SandboxGitWrite)
	}
	if cfg.ApprovalPolicy != "" && cfg.ApprovalPolicy != "never" {
		return nil, fmt.Errorf("%w (only approval \"never\" is supported: sidecar seats cannot ask for approval)", describe())
	}
	mode := cfg.SandboxMode
	if mode == "" {
		if cfg.Provider != ProviderCodex {
			if cfg.SandboxGitWrite || len(cfg.SandboxWritableRoots) > 0 {
				return nil, fmt.Errorf("%w (git write and writable roots need SandboxMode workspace-write)", describe())
			}
			return nil, nil
		}
		mode = omp.SandboxReadOnly
	}
	switch mode {
	case codexSandboxFullAccess:
		return nil, nil
	case omp.SandboxReadOnly:
		if cfg.SandboxGitWrite || len(cfg.SandboxWritableRoots) > 0 {
			return nil, fmt.Errorf("%w (a read-only seat writes nothing, so git write and writable roots do not apply)", describe())
		}
		return &omp.Sandbox{Mode: omp.SandboxReadOnly}, nil
	case omp.SandboxWorkspaceWrite:
		sb := &omp.Sandbox{Mode: omp.SandboxWorkspaceWrite, WritableRoots: slices.Clone(cfg.SandboxWritableRoots)}
		if root := bullseyeLedgerRoot(); root != "" && !slices.Contains(sb.WritableRoots, root) {
			sb.WritableRoots = append(sb.WritableRoots, root)
		}
		gitRoots, err := codexGitWritableRoots(cfg.WorkDir)
		if err != nil {
			if cfg.SandboxGitWrite {
				return nil, fmt.Errorf("omp: sandbox git write for %s: %w", cfg.WorkDir, err)
			}
			gitRoots = nil
		}
		if cfg.SandboxGitWrite {
			// A workdir git answers "not a repository" for has no git
			// directory to grant, and none to protect either: the grant is
			// empty, as it is on the direct Codex route (codexSandboxTuningFor,
			// TestT112DefaultIsQuietOutsideARepository; 🎯T200). Only an unresolvable
			// workdir is refused, above. Refusing here turned every code
			// worker spawned outside a repository into a failed launch.
			sb.WritableRoots = append(sb.WritableRoots, gitRoots...)
		} else {
			sb.Protected = gitRoots
		}
		return sb, nil
	default:
		return nil, describe()
	}
}

// bullseyeLedgerRoot is the bullseye ledger directory every workspace-write
// sidecar seat may write: seats record target status there. Only this one
// directory, and never for read-only seats.
func bullseyeLedgerRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "bullseye")
}

// normalizeSeatConfig preserves provider-specific policy defaults while making
// the omitted provider an explicit Claude plan. CLI-only options cannot be
// silently discarded by the sidecar.
func normalizeSeatConfig(cfg Config) (Config, error) {
	if cfg.Provider == "" {
		cfg.Provider = ProviderClaude
	}
	if !useOMP(cfg) {
		return cfg, nil
	}
	var unsupported []string
	capability := CapabilityExtraArgs
	if cfg.PermissionMode != "" && cfg.PermissionMode != "bypassPermissions" {
		unsupported = append(unsupported, "PermissionMode")
		capability = CapabilityPermissionMode
	}
	if len(cfg.DisallowTools) != 0 {
		unsupported = append(unsupported, "DisallowTools")
		capability = CapabilityToolRestrictions
	}
	if len(cfg.ExtraArgs) != 0 {
		unsupported = append(unsupported, "ExtraArgs")
	}
	if cfg.MCPConfig != "" {
		unsupported = append(unsupported, "MCPConfig (use MCPServers)")
		capability = CapabilityMCPTools
	}
	if cfg.GrokConnect || cfg.ConnectURL != "" || cfg.ConnectPID != 0 {
		unsupported = append(unsupported, "GrokConnect/ConnectURL/ConnectPID")
		capability = CapabilityResume
	}
	if cfg.TermLogPath != "" && cfg.TermLogPath != "-" {
		unsupported = append(unsupported, "TermLogPath")
		capability = CapabilityTerminalLog
	}
	if len(unsupported) != 0 {
		return cfg, unsupportedCapability(cfg.Provider, capability, "sidecar seat cannot honour legacy CLI configuration: "+strings.Join(unsupported, ", "))
	}
	return cfg, nil
}

// withDefaultOMPModel resolves an omitted model before either a broker grant
// or a direct start records the seat. An adopt reports the broker's persisted
// model instead of choosing a potentially different current default.
func withDefaultOMPModel(ctx context.Context, cfg Config) (Config, error) {
	var err error
	cfg, err = normalizeSeatConfig(cfg)
	if err != nil {
		return cfg, err
	}
	if _, err := ompSandboxFor(cfg); err != nil {
		return cfg, err
	}
	if !useOMP(cfg) || cfg.Model != "" || cfg.AdoptOnly {
		return cfg, nil
	}
	model, err := providerLocalSessionModel(ctx, cfg.Provider)
	if err != nil {
		return cfg, err
	}
	cfg.Model = model
	return cfg, nil
}

// standardSessionModel is the standard-quality session model on provider's
// plan, for a seat started or switched without naming one.
func standardSessionModel(ctx context.Context, provider Provider) (string, error) {
	pick, err := Resolve(ctx, ModelPredicates{
		Mode: CapabilitySession, Quality: ModelQualityStandard,
		PreferPlan: true, PreferProvider: provider,
		Usage: []PlanUsage{},
	})
	if err != nil {
		return "", fmt.Errorf("choose standard session model: %w", err)
	}
	if pick.Provider != provider {
		return "", fmt.Errorf("no standard session model on provider %s", provider)
	}
	return pick.Model, nil
}

func providerLocalSessionModel(ctx context.Context, provider Provider) (string, error) {
	model, err := standardSessionModel(ctx, PlanProvider(provider))
	if err != nil {
		return "", fmt.Errorf("%w on %s: %v", ErrNoSubscriptionModel, provider, err)
	}
	if model == "" {
		return "", fmt.Errorf("%w on %s: resolved an empty model id", ErrNoSubscriptionModel, provider)
	}
	return model, nil
}

// ompKeychain is the Keychain command runner. Tests replace it. A nil
// runner refuses rather than reading an API key from the environment.
var ompKeychain omp.Runner

// ompKeychainStdin writes the plan item through stdin. Tests that
// replace ompKeychain leave this nil so Flush does not exec security.
// Production uses omp.ExecSecurityStdin so tokens are not in argv (🎯T131).
var ompKeychainStdin omp.StdinRunner

// ompLogin is the pi-ai helper. Tests replace it. A zero value uses bun
// sidecar/auth.ts.
var ompLogin omp.Login

type ompAgentBackend struct{}

func (ompAgentBackend) Capabilities() providerCapabilities {
	return providerCapabilities{Session: true, Resume: true}
}

func (ompAgentBackend) StartAgent(req agentStartRequest) (*agentStart, error) {
	provider := ompProviderID(req.Config.Provider)
	if provider == "" {
		return nil, fmt.Errorf("omp: %s is not a sidecar provider", req.Config.Provider)
	}
	sandbox, err := ompSandboxFor(req.Config)
	if err != nil {
		return nil, err
	}
	// Adopting a seat that is already running must not mint one, and must
	// not start a sidecar in order to discover that it is not there (🎯T869).
	socket := os.Getenv(omp.SocketEnv)
	if req.Config.AdoptOnly {
		if socket == "" || !omp.Listening(req.Context, socket) {
			return nil, fmt.Errorf("%w: %s", ErrNoSessionWindow, req.Config.Name)
		}
	} else if socket == "" {
		var err error
		socket, err = omp.Ensure(req.Context)
		if err != nil {
			return nil, err
		}
	}
	token, err := ensureOMPToken(req.Context, provider, "launch "+req.Config.Name)
	if err != nil {
		return nil, err
	}
	conn, err := omp.Dial(req.Context, socket)
	if err != nil {
		return nil, err
	}
	op := omp.OpLoad
	if req.Config.AdoptOnly {
		op = omp.OpAdopt
	}
	var tools json.RawMessage
	var toolRoutes, toolNames map[string]string
	var mcpUnavailable []string
	tools, toolRoutes, toolNames, mcpUnavailable = hostToolsReport(req.Context, req.Config.MCPServers)
	// The session names the seat's conversation. The sidecar keeps it on
	// disk under this id, so a sidecar restart resumes it and a new session
	// starts fresh (🎯T151).
	sessionID := req.Config.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	load := omp.Message{
		Op:            op,
		Seat:          req.Config.Name,
		Provider:      provider,
		Model:         req.Config.Model,
		Token:         token,
		Cwd:           req.Config.WorkDir,
		SessionID:     sessionID,
		RequireResume: req.Config.RequireResume,
		Preserve:      req.Config.ContextPreserve,
		Pins:          req.Config.ContextPins,
		Tools:         tools,
		Sandbox:         sandbox,
		ProtectedPaths: ompProtectedPathRules(req.Config),
	}
	if err := conn.Send(load); err != nil {
		conn.Close()
		return nil, err
	}
	if req.Config.AdoptOnly {
		// A wedged sidecar must not hold the resume.
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	}
	ev, err := conn.Recv()
	if req.Config.AdoptOnly {
		_ = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		conn.Close()
		if req.Config.AdoptOnly {
			return nil, fmt.Errorf("%w: %s", ErrNoSessionWindow, req.Config.Name)
		}
		return nil, err
	}
	// jevons 🎯T1096: the name is loaded in the sidecar under another session — a
	// seat removed while its removal never reached the sidecar, or one this
	// name held before it was registered afresh. A fresh start is a new seat
	// under that name: the stale one is unloaded and the load is sent again.
	// Its conversation stays on disk under its own session. A start that
	// requires its conversation replaces nothing.
	if ev.Type != "ready" && ev.Reason == "seat_identity_mismatch" && op == omp.OpLoad && !req.Config.RequireResume {
		slog.Warn("omp: replacing a stale sidecar seat loaded under another session",
			"seat", req.Config.Name, "session", sessionID)
		ev, err = replaceStaleOMPSeat(conn, load)
		if err != nil {
			conn.Close()
			return nil, err
		}
	}
	if ev.Type != "ready" {
		conn.Close()
		if ev.Reason == "seat_identity_mismatch" {
			return nil, fmt.Errorf("%w: seat %q is already loaded there, and this start requires the conversation of session %s, so it was not replaced; remove the seat (jevons_agent_kill) so the name starts fresh, or start under another name: %s",
				ErrSeatIdentityMismatch, req.Config.Name, sessionID, ev.Text)
		}
		if req.Config.AdoptOnly {
			return nil, fmt.Errorf("%w: %s", ErrNoSessionWindow, req.Config.Name)
		}
		refused := fmt.Errorf("omp: sidecar said %q, want ready", ev.Type)
		if ev.Text != "" {
			refused = fmt.Errorf("omp: sidecar said %q, want ready: %s", ev.Type, ev.Text)
		}
		return nil, explainOMPSidecarRefusal(refused, req.Config.Provider, socket)
	}
	if req.Config.RequireResume && !ev.ResumeChecked {
		conn.Close()
		return nil, fmt.Errorf("omp: existing conversation required but sidecar did not confirm resume admission for %s/%s", req.Config.Name, sessionID)
	}
	// A ready response is not proof that the tools in the load reached the
	// model. Refuse rather than silently launching a seat missing host tools.
	if len(toolRoutes) > 0 {
		bound := make(map[string]bool, len(ev.BoundTools))
		for _, name := range ev.BoundTools {
			bound[name] = true
		}
		var missing []string
		for name := range toolRoutes {
			if !bound[name] {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			conn.Close()
			return nil, fmt.Errorf("omp: seat %q sidecar ready but host tools not bound to model: %s", req.Config.Name, strings.Join(missing, ", "))
		}
	}
	if sandbox != nil && ev.Sandbox != sandbox.Mode {
		conn.Close()
		return nil, fmt.Errorf("%w: seat %q asked %q, sidecar applied %q (sidecar predates 🎯T178?)",
			ErrOMPSandboxNotHonoured, req.Config.Name, sandbox.Mode, ev.Sandbox)
	}
	historyLost := ompHistoryLost(ev)
	ctrl := &ompControl{
		conn: conn, bytes: make(chan []byte, 8),
		token: token, provider: provider,
		seat: req.Config.Name, model: req.Config.Model, cwd: req.Config.WorkDir,
		sessionID: sessionID,
		preserve:  req.Config.ContextPreserve, pins: req.Config.ContextPins,
		tools: tools, toolServers: toolRoutes, toolNames: toolNames,
		sandbox: sandbox,
	}
	// A reattach (adopt, or a load that found this seat already running in
	// the sidecar's memory) may find a turn already open from before this
	// host process existed (🎯T202). ev.TurnID names it when the sidecar
	// reports it (seat.ts openTurnID -> server.ts ready reply). "adopted"
	// and "switched" are the sidecar's own words for those two reattach
	// shapes (server.ts), distinct from "launched" for a brand new seat
	// that cannot have a turn yet — and from an empty/unset How, which a
	// bare fake-sidecar reply in older tests uses and which is not a claim
	// about reattach either way.
	//
	// A sidecar old enough to omit turn_id on a reattach, and a sidecar new
	// enough to report "" because the reattached seat really is idle,
	// marshal identically; the two are not distinguishable here, and the
	// 2026-10-20 T1091 incident (an owner message queued six minutes behind
	// a turn the host believed did not exist) is the cost of guessing wrong
	// in the other direction. So any reattach ("adopted"/"switched") biases
	// in-flight true regardless of whether ev.TurnID itself is empty; only
	// an actual turn_end/error narrows it back to idle (🎯T202 safe bias).
	if ev.How == "adopted" || ev.How == "switched" {
		ctrl.setOpenTurn(true, ev.TurnID)
	}
	ompSeats.Store(ctrl, struct{}{})
	return &agentStart{
		Control:            ctrl,
		SessionID:          sessionID,
		HistoryLost:        historyLost,
		HostMCPUnavailable: mcpUnavailable,
		Ops: agentOps{
			explicitRequestIDs: true,
			usagePage: func(ctx context.Context, _ *Agent, after uint64, limit int) (*UsagePage, error) {
				return QueryUsageDirect(ctx, UsageQuery{Seat: ctrl.seat, SessionID: sessionID, After: after, Limit: limit})
			},
			receipt: func(ctx context.Context, _ *Agent, id string) (*RequestReceipt, error) {
				return ctrl.receipt(ctx, id)
			},
			// A sidecar seat runs under its seat id (xai-oauth, anthropic,
			// openai-codex), which the provider turn-caps table does not
			// name, so it reported no steer at all although steer and abort
			// are wired below: a host escalating an owner message to a busy
			// seat held it behind the turn instead (jevons J35, 2026-10-05).
			// The sidecar hands steer text to the agent, which reads it at
			// its next breakpoint between steps.
			turnCaps: func(*Agent) TurnCaps {
				return TurnCaps{
					CanInterrupt:       true,
					CanSteer:           true,
					SteerPolicy:        SteerBreakpoint,
					BusyOnSecondSubmit: BusySubmitReject,
				}
			},
			send: func(a *Agent, text string) error {
				ctrl.inflight.Store(true)
				return ctrl.send(promptMessage(omp.OpPrompt, req.Config.Name, text, a, ctrl))
			},
			sendWithRequestID: func(a *Agent, text, id string) error {
				if id == "" {
					ctrl.inflight.Store(true)
				}
				return ctrl.sendRequest(promptMessage(omp.OpPrompt, req.Config.Name, text, a, ctrl), id)
			},
			steer: func(a *Agent, text string) (DeliveryOutcome, error) {
				err := ctrl.send(promptMessage(omp.OpSteer, req.Config.Name, text, a, ctrl))
				return DeliveryOutcome{}, err
			},
			steerWithRequestID: func(a *Agent, text, id string) (DeliveryOutcome, error) {
				return DeliveryOutcome{}, ctrl.sendRequest(promptMessage(omp.OpSteer, req.Config.Name, text, a, ctrl), id)
			},
			interrupt: func(*Agent) error {
				return ctrl.send(omp.Message{Op: omp.OpAbort, Seat: req.Config.Name})
			},
			switchModel: func(_ *Agent, provider Provider, model string) error {
				return ctrl.switchModel(provider, model)
			},
			promptInFlight: func(*Agent) bool { return ctrl.inflight.Load() },
			openTurnID:     func(*Agent) string { return ctrl.openTurnID() },
			// A removed seat is unloaded from the sidecar, which otherwise
			// keeps it, and its turn, running after the host lets go
			// (jevons 🎯T1096). It names its own session, so a successor already
			// loaded under the name is not dropped.
			discard: func(*Agent) {
				_ = ctrl.send(omp.Message{Op: omp.OpDrop, Seat: ctrl.seat, SessionID: sessionID})
			},
			stop: func(*Agent) {
				ompSeats.Delete(ctrl)
				conn.Close()
			},
		},
		DetectReady: func(a *Agent) {
			go ctrl.pump(a)
			select {
			case <-a.ready:
			default:
				close(a.ready)
			}
		},
		Cleanup: func() {
			ompSeats.Delete(ctrl)
			conn.Close()
		},
	}, nil
}

// replaceStaleOMPSeat unloads whatever seat holds load's name in the
// sidecar and sends load again, returning the sidecar's answer to it
// (jevons 🎯T1096). The drop names no session: the stale seat's is not known here.
func replaceStaleOMPSeat(conn *omp.Conn, load omp.Message) (omp.Event, error) {
	_ = conn.SetDeadline(time.Now().Add(replaceStaleSeatTimeout))
	if err := conn.Send(omp.Message{Op: omp.OpDrop, Seat: load.Seat}); err != nil {
		return omp.Event{}, err
	}
	// "dropped", or an error when the seat went in the meantime: either way
	// the reload says what the sidecar holds now.
	if _, err := conn.Recv(); err != nil {
		return omp.Event{}, fmt.Errorf("omp: unloading stale seat %q: %w", load.Seat, err)
	}
	// The reload waits as long as the first load did.
	_ = conn.SetDeadline(time.Time{})
	if err := conn.Send(load); err != nil {
		return omp.Event{}, err
	}
	return conn.Recv()
}

// replaceStaleSeatTimeout bounds unloading a stale seat, so a wedged
// sidecar fails the start rather than holding it.
const replaceStaleSeatTimeout = 10 * time.Second

type ompControl struct {
	conn      *omp.Conn
	bytes     chan []byte
	token     string
	provider  string
	seat      string
	model     string
	cwd       string
	sessionID string
	// waits routes transport replies. Receipt RPCs use a separate key prefix
	// from legacy switches (whose wire historically calls the RPC request_id).
	waits map[string]chan omp.Event
	// done closes when the event pump loses the sidecar connection.
	done chan struct{}
	// preserve and pins steer the seat's compaction (🎯T152).
	preserve string
	pins     []string
	// tools is the host tool list the seat was loaded with; a reload keeps it.
	tools json.RawMessage
	// sandbox is the seat's tool policy; a reload sends it again (🎯T178).
	sandbox *omp.Sandbox
	// toolServers routes a model tool name to the AgentDef.MCPServers URL
	// that declared it (🎯T871.1). A name absent here (a jevons_* name the
	// host did not list, or a legacy caller) falls back to runOMPTool,
	// which refuses unless the process installed an explicit runner.
	toolServers map[string]string
	// toolNames maps an advertised tool name to the server's own name for
	// it, where the provider needed it renamed (🎯T146).
	toolNames map[string]string
	mu        sync.Mutex
	inflight  atomic.Bool
	// turnID is the running turn's id, when known (🎯T202). Set alongside
	// inflight so a reattach that learns of a turn from the sidecar's ready
	// reply, not just from a later accepted/receipt, can name it too. Read
	// and written under mu; "" does not by itself mean idle — check
	// inflight.
	turnID string
	// lastRefresh is when this seat last refreshed a rejected token (under mu).
	lastRefresh time.Time
}

// ompSeats holds the live sidecar seats, so a plan recovered by the owner
// reaches the seats already running on it (🎯T141).
var ompSeats sync.Map // *ompControl -> struct{}

// ompRefreshBackoff bounds how often one seat refreshes a rejected token. It
// replaces a once-per-lifetime guard: a seat whose refresh failed before the
// owner repaired the plan must be able to recover afterwards (🎯T141).
const ompRefreshBackoff = time.Minute

// ompPlanRenewQuiet is how long after a plan's login was renewed a refusal is
// taken as one of the token that renewal replaced (🎯T165). A request already
// in flight on the old token fails after the seat has been moved to the new
// one, so the seat's current token is not the token that was refused. On
// 2026-10-01 each such refusal refreshed again, revoking the token under
// every other seat: 34 rotations in 25 minutes, every one a seat's.
const ompPlanRenewQuiet = 2 * time.Minute

func promptMessage(op, seat, text string, a *Agent, ctrl *ompControl) omp.Message {
	cause, detail, resume, turnID, sessionID := "", "", "", "", ""
	if c, ok := a.armedPromptCause(); ok {
		cause, detail, resume = c.Cause, c.Detail, c.Resume
		turnID, sessionID = c.TurnID, c.SessionID
	}
	if op == omp.OpSteer && cause == "" {
		cause = omp.CauseSteer
	}
	if cause == "" || !omp.ValidCause(cause) {
		cause, detail = omp.ClassifyCause(text)
	}
	if detail == "" {
		detail = omp.OneLine(text)
	} else {
		detail = omp.OneLine(detail)
	}
	if !omp.ValidResume(resume) {
		resume = ""
	}
	if sessionID == "" && a != nil {
		sessionID = a.SessionID()
	}
	if sessionID == "" && ctrl != nil {
		sessionID = ctrl.sessionID
	}
	if turnID == "" {
		turnID = uuid.NewString()
	}
	return omp.Message{
		Op: op, Seat: seat, Text: text,
		TurnID: turnID, SessionID: sessionID,
		Cause: cause, CauseDetail: detail, Resume: resume,
	}
}

func (c *ompControl) send(msg omp.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Send(msg)
}

// setOpenTurn records whether a turn is open and, when known, its id
// (🎯T202). inflight is the atomic callers already read through
// promptInFlight; turnID is kept alongside it under mu so the two never
// disagree about which turn, if any, is running.
func (c *ompControl) setOpenTurn(open bool, turnID string) {
	c.inflight.Store(open)
	c.mu.Lock()
	if open {
		if turnID != "" {
			c.turnID = turnID
		}
	} else {
		c.turnID = ""
	}
	c.mu.Unlock()
}

// openTurnID is the Ops.openTurnID hook (🎯T202). "" means idle, or a turn
// is open but the sidecar has not yet named it (reattach to an old sidecar,
// or to a genuinely idle seat that marshal identically) — callers check
// inflight (via PromptInFlight) rather than treat "" as proof of idle.
func (c *ompControl) openTurnID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turnID
}

func (c *ompControl) pump(a *Agent) {
	defer close(c.bytes)
	c.mu.Lock()
	if c.done == nil {
		c.done = make(chan struct{})
	}
	done := c.done
	c.mu.Unlock()
	defer close(done)
	// Recv and publication run in this one goroutine, so the sequence belongs
	// to the pump rather than the control's concurrently accessed send state.
	var fragments ompFragmentSequence
	for {
		ev, err := c.conn.Recv()
		if err != nil {
			return
		}
		if raw, err := json.Marshal(ev); err == nil {
			select {
			case c.bytes <- raw:
			default:
			}
		}
		// Transport replies use rpc_id. Never let a terminal event for a
		// different logical input satisfy an identified send or query.
		if ev.RPCID != "" {
			if ch := c.takeWait("rpc:" + ev.RPCID); ch != nil {
				ch <- ev
			}
			if ev.Type != "accepted" {
				continue
			}
		}
		// A reply to a switch is the switch's, not a turn's (🎯T195): a
		// refusal must not read as a failed turn.
		if ev.RequestID != "" && (ev.Type == "ready" || ev.Type == "error") {
			if ch := c.takeWait(ev.RequestID); ch != nil {
				ch <- ev
				continue
			}
		}
		switch ev.Type {
		case "usage":
			if err := ev.UsageFact.ValidateIdentity(c.seat, c.sessionID); err != nil {
				a.publishEvent(Event{Type: "usage_error", Text: err.Error(), SessionID: c.sessionID})
				continue
			}
			RecordObservedUsage(ev.UsageFact)
			a.publishEvent(Event{Type: "usage", SessionID: c.sessionID, UsageFact: ev.UsageFact})
		case "usage_error":
			a.publishEvent(Event{Type: "usage_error", Text: ev.Text, SessionID: c.sessionID})
		case "receipt":
			if err := ev.Receipt.ValidateIdentity(c.seat, c.sessionID, ev.RequestID); err != nil {
				slog.Warn("omp refused receipt", "seat", c.seat, "error", err)
				continue
			}
			RecordObservedReceipt(ev.Receipt)
			if ev.Receipt.State == ReceiptStarted {
				c.setOpenTurn(true, ev.TurnID)
			}
			a.publishEvent(Event{Type: "receipt", RequestID: ev.RequestID, SessionID: c.sessionID, Receipt: ev.Receipt})
		case "auth_retry":
			if ev.RequestID != "" && ev.TurnID != "" && len(ev.FailedToken) == 64 && oauthRejected(ev.Error) {
				go c.answerAuthRetry(ev)
			}
		case "accepted":
			if ev.RequestID != "" {
				if err := ev.Receipt.ValidateIdentity(c.seat, c.sessionID, ev.RequestID); err != nil {
					slog.Warn("omp refused acceptance receipt", "seat", c.seat, "error", err)
					continue
				}
				RecordObservedReceipt(ev.Receipt)
				if ev.Receipt.State != ReceiptAccepted && ev.Receipt.State != ReceiptStarted {
					// Replaying an ACK for a terminal or uncertain obligation
					// does not mean that a new model turn was accepted.
					a.publishEvent(Event{Type: "receipt", RequestID: ev.RequestID, SessionID: c.sessionID, Receipt: ev.Receipt})
					continue
				}
			}
			// The sidecar took the prompt (a turn began, or it was queued
			// behind the running one). Before the first token this is the
			// only sign the prompt landed. A turn is open from here: that
			// includes the turn a hard-stop starts to deliver what was
			// queued (🎯T138), which no send of ours opened.
			if ev.Receipt == nil || ev.Receipt.State == ReceiptAccepted || ev.Receipt.State == ReceiptStarted {
				c.setOpenTurn(true, ev.TurnID)
			}
			a.publishEvent(Event{Type: "progress", ProgressType: ProgressPromptAccepted, TurnID: ev.TurnID, RequestID: ev.RequestID, Receipt: ev.Receipt})
		case "absorbed":
			// The model took a message queued or steered behind the turn;
			// an escalation for it stops here (🎯T138).
			a.publishEvent(Event{Type: "progress", ProgressType: ProgressDeliveryAbsorbed, Text: ev.Text, TurnID: ev.TurnID, RequestID: ev.RequestID})
		case "compacting", "compacted", "compaction_warning":
			// The seat is managing its context (🎯T150): a host can show
			// when and why a long conversation was folded, or that it could
			// not be.
			slog.Info("omp seat context maintenance", "seat", c.seat, "event", ev.Type, "text", ev.Text)
			a.publishEvent(Event{Type: "progress", ProgressType: ProgressCompaction, Text: ev.Type + ": " + ev.Text})
		case "text":
			visible, token := omp.StripStopToken(ev.Text)
			if visible == "" && token != "" {
				break
			}
			if err := publishOMPAnswer(a, ev, Event{
				Type:          "assistant",
				Text:          visible,
				PreviewUpdate: PreviewUpdateAppend,
			}, &fragments); err != nil {
				slog.Error("omp refused sidecar answer", "seat", c.seat, "error", err)
			}
		case "tool_call":
			a.publishEvent(Event{
				Type:         "progress",
				ProgressType: "tool_use",
				ToolCallID:   ev.CallID,
				ToolTitle:    ev.Name,
				Text:         ev.Text,
			})
			// Off the pump (jevons 🎯T887): a host tool can take tens of
			// seconds (a slow jevons_* call), and run inline it held back
			// every later event from this seat — an acceptance of a new
			// prompt among them, which a host then reported undelivered. The
			// result goes back by call id, so order does not matter.
			go func(name, callID, args string) {
				result := c.runTool(name, callID, args)
				_ = c.send(omp.Message{Op: omp.OpTool, CallID: callID, Result: result})
			}(ev.Name, ev.CallID, ev.Text)
		case "tool_start":
			// 🎯T171: one of the sidecar's own tools (Bash, Read, …) started.
			// Nothing for the host to run; it is published so a seat working
			// in a shell is visible, in the shape a host tool's call has.
			a.publishEvent(Event{
				Type:         "progress",
				ProgressType: "tool_use",
				ToolCallID:   ev.CallID,
				ToolTitle:    ev.Name,
				Text:         ev.Text,
			})
		case "aborted":
			// An abort found no turn open: nothing ended, so nothing is
			// answered. The turn an abort does end arrives as its turn_end,
			// under its own turn_id.
		case "seat_error":
			// A failure that belongs to no turn and no waiter (an abort of a
			// seat the sidecar no longer holds, a queue drain that could not
			// begin a turn). It is said, but it is not any turn's answer:
			// publishing it as one ended whichever wait happened to be open.
			slog.Error("omp sidecar seat error", "seat", c.seat, "reason", ev.Reason, "error", ev.Text)
			a.publishEvent(Event{Type: "system", Text: "omp sidecar: " + ev.Text, SessionID: c.sessionID})
		case "turn_end", "error":
			c.setOpenTurn(false, "")
			// Only this turn's own error says whether the token was refused
			// (🎯T165). The snapshot is the whole conversation and a turn
			// end's text is the reply: a seat whose history holds an old 401,
			// or whose answer discusses one, matched on every turn and renewed
			// the plan's token under the whole fleet (2026-10-01).
			why := ""
			switch ev.Type {
			case "error":
				why = ev.Text
			case "turn_end":
				why = ev.Refusal()
			}
			rejected := oauthRejected(why)
			if rejected {
				c.recoverRejectedToken()
			}
			// A refused turn (usage limit, rate limit, auth) ends with no
			// text. Said as an error, WaitForResponse names the refusal
			// instead of returning an empty answer (🎯T137).
			text := ev.Text
			refusal := ev.Refusal()
			if refusal != "" {
				text = "provider refused the turn: " + refusal
			}
			// The sidecar has already compacted and retried once; an
			// overflow it still reports is terminal (🎯T148).
			reason := ""
			if refusal != "" && ev.Reason == ReasonContextOverflow {
				reason = ReasonContextOverflow
			}
			// An aborted turn ends as itself, with no reply: it ends like any
			// turn (end_turn) and says how in Reason, so the transcript paints
			// nothing for it (jevons 2026-10-21: the word "aborted" in Text
			// reached the owner as an answer).
			if ev.StopReason == ReasonAborted && reason == "" {
				reason = ReasonAborted
			}
			if err := publishOMPAnswer(a, ev, Event{
				Type:       "assistant",
				Text:       text,
				IsError:    rejected || ev.Type == "error" || refusal != "",
				StopReason: "end_turn",
				Reason:     reason,
			}, &fragments); err != nil {
				slog.Error("omp refused sidecar answer", "seat", c.seat, "error", err)
			}
		}
	}
}

// ompFragmentSequence belongs to one pump. A changed TurnID starts a new
// one-based fragment stream, including a terminal event even if it is the
// only event of that turn.
type ompFragmentSequence struct {
	turnID string
	last   int
}

// publishOMPAnswer validates the sidecar identity before any subscriber or
// broker sees its text or terminal payload. A distinct error event lets
// WaitForResponse fail rather than waiting forever for a discarded terminal.
func publishOMPAnswer(a *Agent, wire omp.Event, answer Event, fragments *ompFragmentSequence) error {
	if strings.TrimSpace(wire.TurnID) == "" {
		err := fmt.Errorf("%w: %s", ErrOMPMissingTurnID, wire.Type)
		a.publishEvent(Event{Type: "assistant", IsError: true, StopReason: "end_turn", Text: err.Error()})
		return err
	}
	if fragments.turnID != wire.TurnID {
		fragments.turnID = wire.TurnID
		fragments.last = 0
	}
	fragments.last++
	answer.Final = wire.Type == "turn_end" || wire.Type == "error"
	answer.FragmentSeq = fragments.last
	answer.TurnID = wire.TurnID
	answer.RequestID = wire.RequestID
	a.publishEvent(answer)
	return nil
}

// oauthRejected reports a provider refusal of the access token, on the
// error event or inside the turn snapshot. A bad refresh token for a
// different plan is not this signal (🎯T868).
func oauthRejected(blob string) bool {
	blob = strings.ToLower(blob)
	if blob == "" {
		return false
	}
	for _, n := range []string{
		"could not be validated",
		"unauthenticated:bad-credentials",
		"invalid_token",
		"authentication_error",
		"401 oauth access token has expired",
	} {
		if strings.Contains(blob, n) {
			return true
		}
	}
	return false
}

// recoverRejectedToken gets this seat a working token after the provider
// rejected its own. A token already repaired in the plan store (the owner's
// reauth, or another seat's refresh) is taken as is; otherwise the seat
// refreshes, at most once per ompRefreshBackoff.
func (c *ompControl) recoverRejectedToken() {
	c.recoverFailedToken(tokenFingerprint(c.currentToken()))
}

func tokenFingerprint(token string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(token))) }

// The sidecar identifies the credential actually used, even if renewal raced the request.
func (c *ompControl) recoverFailedToken(failed string) {
	// One refresh at a time per plan (🎯T158). A refresh spends the refresh
	// token it presents, so two seats refused together that both refresh
	// present the same one: the second gets invalid_grant, and that marks a
	// healthy plan rejected and asks the owner to sign in again. The seat
	// that waited finds the first one's token below and takes it.
	// Compare with the token that was refused, not the seat's current one:
	// the refresh this seat waited behind has already reloaded it.
	mu := ompRefreshLock(c.provider)
	mu.Lock()
	defer mu.Unlock()
	tok, err := planStore().AccessToken(context.Background(), c.provider)
	if err == nil && tokenFingerprint(tok) != failed {
		slog.Info("omp token rejected; seat takes the plan's newer token", "provider", c.provider, "seat", c.seat)
		c.reload(tok)
		return
	}
	if at := omp.LastRenewal(c.provider); err == nil && !at.IsZero() && time.Since(at) < ompPlanRenewQuiet {
		slog.Info("omp token rejected; plan renewed moments ago, not rotating it again",
			"provider", c.provider, "seat", c.seat, "renewed_ago", time.Since(at).Round(time.Second))
		return
	}
	c.mu.Lock()
	if !c.lastRefresh.IsZero() && time.Since(c.lastRefresh) < ompRefreshBackoff {
		c.mu.Unlock()
		// 🎯T167: a refusal the backoff swallows used to leave no trace.
		slog.Info("omp token rejected; refresh held by backoff", "provider", c.provider, "seat", c.seat)
		return
	}
	c.lastRefresh = time.Now()
	c.mu.Unlock()
	c.refreshRejectedToken()
}

// A response is useful only to the matching pending sidecar turn. The token
// travels on the private command wire, never on the event/spool path.
func (c *ompControl) answerAuthRetry(ev omp.Event) {
	c.recoverFailedToken(ev.FailedToken)
	reply := omp.Message{Op: omp.OpAuthRetry, Seat: c.seat, RequestID: ev.RequestID, TurnID: ev.TurnID}
	item, err := planStore().Load(context.Background())
	if err == nil {
		rec := item.Records[c.provider]
		if rec.AccessToken != "" && tokenFingerprint(rec.AccessToken) != ev.FailedToken && rec.Expiry.After(time.Now()) {
			reply.Token = rec.AccessToken
			reply.ExpiresAt = rec.Expiry.UnixMilli()
		}
	}
	_ = c.send(reply) // Missing delivery times out closed at the sidecar.
}

func (c *ompControl) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// switchModel moves this seat to provider and model in place (🎯T195): a
// load naming them, which the sidecar answers once the seat's agent has taken
// the new model (folding the conversation first when it would not fit), or
// refuses with the seat left as it was. The seat's provider, model and token
// change only once the sidecar has switched.
func (c *ompControl) switchModel(provider Provider, model string) error {
	id := ompProviderID(provider)
	if id == "" {
		return fmt.Errorf("omp: %s is not a sidecar provider", provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), switchModelTimeout)
	defer cancel()
	c.mu.Lock()
	token, same := c.token, id == c.provider
	c.mu.Unlock()
	if !same {
		var err error
		if token, err = ensureOMPToken(ctx, id, "switch "+c.seat); err != nil {
			return err
		}
	}
	requestID := uuid.NewString()
	reply := make(chan omp.Event, 1)
	c.mu.Lock()
	if c.waits == nil {
		c.waits = map[string]chan omp.Event{}
	}
	c.waits[requestID] = reply
	c.mu.Unlock()
	defer c.takeWait(requestID)
	msg := c.loadMessage()
	msg.Provider, msg.Model, msg.Token, msg.RequestID = id, model, token, requestID
	if err := c.send(msg); err != nil {
		return err
	}
	select {
	case ev := <-reply:
		if ev.Type == "error" {
			return fmt.Errorf("sidecar refused the switch to %s/%s: %s", id, model, ev.Text)
		}
		c.mu.Lock()
		c.provider, c.model, c.token = id, model, token
		c.mu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("switch to %s/%s: no answer from the sidecar: %w", id, model, ctx.Err())
	}
}

// takeWait removes and returns the waiter for requestID, or nil.
func (c *ompControl) takeWait(requestID string) chan omp.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.waits[requestID]
	delete(c.waits, requestID)
	return ch
}

// ensureOMPToken is provider's plan token, logging in when the plan has
// none. caller names who asked, for the login's own record.
func ensureOMPToken(ctx context.Context, provider, caller string) (string, error) {
	login := ompLogin
	if login.Run == nil {
		login.Run = execBunLogin
	}
	if login.Command == "" {
		login.Command = "bun"
	}
	if login.Script == "" {
		login.Script = sidecarAuthScript()
	}
	login.Caller = caller
	return planStore().Ensure(ctx, provider, login)
}

// loadMessage is the load that (re)configures this seat with its current
// token, model and host tools.
func (c *ompControl) loadMessage() omp.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return omp.Message{
		Op: omp.OpLoad, Seat: c.seat, Provider: c.provider, SessionID: c.sessionID,
		Preserve: c.preserve, Pins: c.pins,
		Model: c.model, Token: c.token, Cwd: c.cwd,
		Tools: c.tools, Sandbox: c.sandbox,
	}
}

// reload hands the seat a new token; its conversation is kept.
func (c *ompControl) reload(token string) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
	if err := c.send(c.loadMessage()); err != nil {
		slog.Warn("omp token refreshed; sidecar reload failed", "seat", c.seat, "err", err)
	}
}

// reloadOMPSeats hands provider's stored token to every live seat still
// holding another one, and says how many it moved. A sidecar that holds one
// token per plan gets it once, as OpToken, and all its seats take it
// (🎯T159); a seat on an older sidecar is reloaded on its own.
func reloadOMPSeats(ctx context.Context, provider string) int {
	tok, err := planStore().AccessToken(ctx, provider)
	if err != nil {
		return 0
	}
	var stale []*ompControl
	ompSeats.Range(func(k, _ any) bool {
		c := k.(*ompControl)
		if c.provider == provider && c.currentToken() != tok {
			stale = append(stale, c)
		}
		return true
	})
	// The sidecar holds one token per plan (🎯T159), and it is always this
	// broker's own build (🎯T166), so one OpToken moves every seat. Every
	// seat's connection reaches that one sidecar; the first that takes the
	// message is enough. A seat whose connection refuses it is reloaded.
	sent := false
	for _, c := range stale {
		if !sent {
			if err := c.send(omp.Message{Op: omp.OpToken, Provider: provider, Token: tok}); err != nil {
				slog.Warn("omp token refreshed; sidecar token update failed", "seat", c.seat, "err", err)
				c.reload(tok)
				continue
			}
			sent = true
		}
		c.mu.Lock()
		c.token = tok
		c.mu.Unlock()
	}
	return len(stale)
}

func (c *ompControl) refreshRejectedToken() {
	login := ompLogin
	if login.Run == nil {
		login.Run = execBunLogin
	}
	if login.Command == "" {
		login.Command = "bun"
	}
	if login.Script == "" {
		login.Script = sidecarAuthScript()
	}
	login.ForceRefresh = true
	login.Caller = "seat " + c.seat + " refused"
	// A seat's refusal is never a person at the keyboard (🎯T165).
	login.NoLogin = true
	rec, err := login.Refresh(context.Background(), planStore(), c.provider)
	if err != nil {
		slog.Warn("omp token rejected; refresh failed", "provider", c.provider, "seat", c.seat, "err", err)
		// 🎯T924: the cockpit offers the owner a Reauth for this plan.
		omp.MarkRejected(c.provider, err.Error())
		return
	}
	if err := FlushOMPPlans(context.Background()); err != nil {
		slog.Warn("omp token refreshed; keychain flush failed", "provider", c.provider, "err", err)
	}
	// Every seat on the plan, this one included, gets the new token now,
	// rather than each on its next refusal (🎯T158, 🎯T159).
	if reloadOMPSeats(context.Background(), c.provider) == 0 && c.currentToken() != rec.AccessToken {
		c.reload(rec.AccessToken)
	}
}

// ompRefreshLocks serialises rejected-token refreshes per plan (🎯T158).
var ompRefreshLocks sync.Map // provider -> *sync.Mutex

func ompRefreshLock(provider string) *sync.Mutex {
	mu, _ := ompRefreshLocks.LoadOrStore(provider, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

func sidecarAuthScript() string {
	if p := os.Getenv("CLAUDIA_OMP_AUTH"); p != "" {
		return p
	}
	if dir := omp.PackagedSidecarDir(); dir != "" {
		return filepath.Join(dir, "auth.ts") // 🎯T157
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "sidecar/auth.ts"
	}
	return filepath.Join(filepath.Dir(file), "sidecar", "auth.ts")
}

// ompToolExec is the Go callback for jevons_* tool calls. Tests replace it.
var ompToolExec func(name, callID, args string) string

// EnsureOMPSidecar starts the Bun sidecar if it is not already listening.
// The sidecar is this process's child and exits with it (🎯T166).
func EnsureOMPSidecar(ctx context.Context) (string, error) {
	return omp.Ensure(ctx)
}

// OpenOMPPlans reads the plan Keychain item once for this process.
func OpenOMPPlans(ctx context.Context) error {
	return omp.Open(ctx, planStore())
}

// FlushOMPPlans writes the plan item once, on the way out, when the
// memory copy differs from the startup read.
func FlushOMPPlans(ctx context.Context) error {
	return omp.Flush(ctx, planStore())
}

func planStore() omp.Store {
	run := ompKeychain
	runStdin := ompKeychainStdin
	if run == nil {
		run = execKeychain
		if runStdin == nil {
			runStdin = omp.ExecSecurityStdin
		}
		if testing.Testing() {
			// A test that did not fake the Keychain must not reach the
			// owner's: a Flush with no key writes a new one (🎯T143).
			run = refuseKeychainInTest
			runStdin = func(ctx context.Context, _ []byte, name string, args ...string) ([]byte, error) {
				return refuseKeychainInTest(ctx, name, args...)
			}
		}
	}
	path := omp.ProductBrokerPath()
	seal := path != ""
	if path == "" {
		path = os.Args[0]
	}
	// An unresolvable path leaves DataPath empty; Flush and a key-holding
	// Open then fail by name rather than write somewhere unexpected.
	data, _ := omp.DefaultDataPath()
	return omp.Store{BrokerPath: path, Run: run, RunStdin: runStdin, SealPath: seal, DataPath: data}
}

// RefreshOMPPlans renews every stored subscription login through
// sidecar/auth.ts / pi-ai and writes the records back (🎯T865).
// Providers with no refresh token are skipped so serve does not open
// a browser. The Keychain ACL is -T this process (os.Args[0]).
func RefreshOMPPlans(ctx context.Context) (refreshed, skipped []string, err error) {
	store := planStore()
	login := ompLogin
	if login.Run == nil {
		login.Run = execBunLogin
	}
	if login.Command == "" {
		login.Command = "bun"
	}
	if login.Script == "" {
		login.Script = sidecarAuthScript()
	}
	if os.Getenv("OMP_FORCE_REFRESH") != "" {
		login.ForceRefresh = true
	}
	login.Caller = "broker boot"
	return omp.RefreshPlans(ctx, store, login)
}

// LoginOMPPlans runs pi-ai login for every plan id that has no refresh
// token (🎯T865). Interactive — not used on serve.
func LoginOMPPlans(ctx context.Context, ids ...string) (int, error) {
	store := planStore()
	login := ompLogin
	if login.Run == nil {
		login.Run = execBunLogin
	}
	if login.Command == "" {
		login.Command = "bun"
	}
	if login.Script == "" {
		login.Script = sidecarAuthScript()
	}
	login.Caller = "login command"
	return omp.LoginPlans(ctx, store, login, ids...)
}

// OMPPlanRecovery names one plan to repair (🎯T165).
type OMPPlanRecovery struct {
	// Plan is a subscription plan id (anthropic, openai-codex, cursor,
	// xai-oauth) or a provider that maps to one.
	Plan string
	// Login lets a refused refresh fall back to an interactive sign-in.
	// Only a person at the keyboard sets it; an unattended repair that
	// needs a sign-in fails with omp.ErrNeedsSignIn and marks the plan
	// rejected, so the owner's Reauth can do it.
	Login bool
}

// RecoverOMPAuth asks the running broker to repair a plan's authentication.
// The broker owns the in-memory Keychain copy; a separate client process must
// never attempt to update that copy itself.
func RecoverOMPAuth(ctx context.Context, r *OMPPlanRecovery) error {
	id := ompProviderID(Provider(r.Plan))
	if id == "" {
		return fmt.Errorf("omp: %s is not a subscription provider", r.Plan)
	}
	client, err := dialBroker()
	if err != nil {
		return fmt.Errorf("omp: broker is required for reauthentication: %w", err)
	}
	defer client.Close()
	resp, err := client.call(ctx, &broker.Request{Type: broker.TypeAuthRecover,
		AuthRecover: &broker.AuthRecoverRequest{Name: id, Login: r.Login}})
	if err != nil {
		return err
	}
	if resp.Type != broker.TypeAuthRecovered || resp.AuthRecovered == nil || resp.AuthRecovered.Name != id {
		return fmt.Errorf("omp: unexpected reauthentication response %q", resp.Type)
	}
	return nil
}

// PlanLoginHealth is one subscription plan's login state (🎯T924).
type PlanLoginHealth = omp.PlanHealth

// OMPPlanHealth reads every plan's login health from this process's plan
// store (🎯T924). It runs inside the broker and never starts a login.
func OMPPlanHealth(ctx context.Context) ([]PlanLoginHealth, error) {
	return omp.Health(ctx, planStore())
}

// OMPAuthStatus asks the running broker for every plan's login health.
func OMPAuthStatus(ctx context.Context) ([]PlanLoginHealth, error) {
	client, err := dialBroker()
	if err != nil {
		return nil, fmt.Errorf("omp: broker is required for plan login status: %w", err)
	}
	defer client.Close()
	resp, err := client.call(ctx, &broker.Request{Type: broker.TypeAuthStatus, AuthStatus: &broker.AuthStatusRequest{}})
	if err != nil {
		return nil, err
	}
	if resp.Type != broker.TypeAuthStatusResult || resp.AuthStatus == nil {
		return nil, fmt.Errorf("omp: unexpected plan login status response %q", resp.Type)
	}
	out := make([]omp.PlanHealth, 0, len(resp.AuthStatus.Plans))
	for _, p := range resp.AuthStatus.Plans {
		out = append(out, omp.PlanHealth{Provider: p.Provider, State: p.State, Detail: p.Detail, Since: p.Since})
	}
	return out, nil
}

// IsOMPPlan reports whether the broker can reauthenticate a plan id.
func IsOMPPlan(provider string) bool { return omp.Subscription(provider) }

// RecoverOMPPlan runs inside the broker, which owns the plan Keychain copy.
// It retries a failed read and repairs the named plan (omp.RecoverPlan): a
// plan already healthy is left alone, and a sign-in opens only when r.Login
// allows it. It holds the plan's refresh lock, the one a refused seat's own
// refresh takes (🎯T158), so the two never present the same refresh token:
// the loser of that race got invalid_grant and fell into a sign-in (🎯T165).
func RecoverOMPPlan(ctx context.Context, r *OMPPlanRecovery) error {
	provider := r.Plan
	login := ompRecoveryLogin(r.Login)
	mu := ompRefreshLock(provider)
	mu.Lock()
	err := omp.RecoverPlan(ctx, planStore(), login, provider)
	mu.Unlock()
	if err != nil {
		return err
	}
	// Seats still holding an older token take the plan's (🎯T141).
	if n := reloadOMPSeats(ctx, provider); n > 0 {
		slog.Info("omp plan recovered; live seats reloaded", "provider", provider, "seats", n)
	}
	return nil
}

func ompRecoveryLogin(interactive bool) omp.Login {
	login := ompLogin
	if login.Run == nil {
		login.Run = execBunLogin
	}
	if login.Command == "" {
		login.Command = "bun"
	}
	if login.Script == "" {
		login.Script = sidecarAuthScript()
	}
	login.NoLogin = !interactive
	login.Caller = "recover (unattended)"
	if interactive {
		login.Caller = "recover (owner)"
	}
	return login
}

// SetOMPToolExec installs the callback the sidecar invokes for a tool
// the seat did not route to a declared server (🎯T865). The broker
// installs [DefaultOMPToolExec], which refuses. A declared
// AgentDef.MCPServers route never reaches this callback.
func SetOMPToolExec(fn func(name, callID, args string) string) {
	ompToolExec = fn
}

// runTool executes one model tool call for this seat. A tool whose home
// server was discovered from AgentDef.MCPServers (🎯T871.1) is routed there
// with a structured tools/call. Any other name goes through runOMPTool,
// which refuses when no runner is installed.
func (c *ompControl) runTool(name, callID, args string) string {
	if c != nil && c.toolServers != nil {
		if url, ok := c.toolServers[name]; ok {
			if orig, renamed := c.toolNames[name]; renamed {
				name = orig
			}
			return callMCPTool(url, name, args, c.seat)
		}
	}
	return runOMPTool(name, callID, args)
}

func runOMPTool(name, callID, args string) string {
	if ompToolExec != nil {
		return ompToolExec(name, callID, args)
	}
	return DefaultOMPToolExec(name, callID, args)
}

// DefaultOMPToolExec refuses an unrouted tool call. A neutral runtime
// has no implicit consumer endpoint: JEVONS_MCP_URL is not read, and
// the development :13705 path is not a default. A declared
// AgentDef.MCPServers route, and a caller that already holds a URL,
// uses [CallMCPTool].
func DefaultOMPToolExec(name, callID, args string) string {
	return fmt.Sprintf("omp: no tool endpoint configured for %q", name)
}

// jevonsSeatHeader names the calling seat. It matches Jevons
// mcpserver.SeatHeader. Claudia does not import Jevons.
const jevonsSeatHeader = "X-Jevons-Agent"

// CallMCPTool POSTs a structured JSON-RPC tools/call to an MCP server
// (🎯T871.1). No name prefix is required: the caller already decided this
// tool belongs to this server, via [hostTools]'s discovered route.
func CallMCPTool(mcpURL, name, args string) string {
	return callMCPTool(mcpURL, name, args, "")
}

func callMCPTool(mcpURL, name, args, seat string) string {
	var arguments any
	if strings.TrimSpace(args) == "" {
		arguments = map[string]any{}
	} else if json.Unmarshal([]byte(args), &arguments) != nil {
		arguments = map[string]any{"text": args}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if err != nil {
		return fmt.Sprintf("omp: encode %s: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The call runs in a session of its own, as the listing does (🎯T147):
	// a server that keeps sessions refuses a bare tools/call, and a stdio
	// server drops one it has not been initialized for.
	session, err := mcpOpenSession(ctx, mcpURL)
	if err != nil {
		return fmt.Sprintf("omp: %s call failed: %v", name, err)
	}
	if session != "" {
		defer mcpEndSession(mcpURL, session)
	}
	raw, _, cause, err := mcpPost(ctx, mcpURL, session, body, seat)
	if cause != "" {
		return fmt.Sprintf("omp: %s call failed (%s): %v", name, cause, err)
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return strings.TrimSpace(string(raw))
	}
	if envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	var b strings.Builder
	for _, c := range envelope.Result.Content {
		b.WriteString(c.Text)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return strings.TrimSpace(string(raw))
	}
	return out
}

func refuseKeychainInTest(context.Context, string, ...string) ([]byte, error) {
	return nil, errors.New("omp: a test binary never touches the real Keychain; set ompKeychain (🎯T143)")
}

func execKeychain(ctx context.Context, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = omp.ScrubEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return stderr.Bytes(), fmt.Errorf("%s: keychain ACL did not approve this binary: %w", name, ctx.Err())
		}
		return stderr.Bytes(), fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return out, nil
}

// execBunLogin runs sidecar/auth.ts through bun from that directory so
// @oh-my-pi/pi-ai resolves. It is not the Keychain runner. Stderr is
// forwarded so an interactive login URL reaches the owner (🎯T865).
func execBunLogin(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := omp.EnsureDeps(ctx, sidecarAuthScript()); err != nil {
		return nil, err
	}
	var stdin []byte
	if len(args) >= 4 {
		stdin = []byte(args[len(args)-1])
		args = args[:len(args)-1]
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = omp.ScrubEnv(os.Environ())
	if dir := filepath.Dir(sidecarAuthScript()); dir != "." && dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	out, err := cmd.Output()
	if err != nil {
		return stderr.Bytes(), fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return out, nil
}

func (c *ompControl) Bytes() <-chan []byte { return c.bytes }
func (c *ompControl) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// hostJevonsTool is one host tool as the sidecar declares it to the model.
type hostJevonsTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// hostTools asks every eligible server in AgentDef.MCPServers for its
// tools/list and returns the model-visible tool set plus a name→server-URL
// route for structured tools/call (🎯T871.1). A server with no http URL —
// stdio, or a plugin-shaped entry that names only a Command — is not
// registered: neither the sidecar nor this host speaks anything but MCP
// over HTTP today. Without this a sidecar seat is never shown its host's
// tools at all, so an overseer on a sidecar provider cannot message a
// product owner and a product owner cannot start a worker (🎯T886). A
// server that offers no tools, or does not answer, contributes nothing;
// the seat still starts. The first server to declare a name wins ties.
func hostTools(ctx context.Context, servers []MCPServer) (json.RawMessage, map[string]string) {
	raw, routes, _ := hostToolsNamed(ctx, servers)
	return raw, routes
}

// hostToolsNamed is hostTools plus, for each advertised name that differs
// from the server's own, the server's name to call it by (🎯T146).
func hostToolsNamed(ctx context.Context, servers []MCPServer) (json.RawMessage, map[string]string, map[string]string) {
	raw, routes, names, _ := hostToolsReport(ctx, servers)
	return raw, routes, names
}

// hostToolsReport is hostToolsNamed plus the servers that listed no tools
// within the launch's budget (jevons 🎯T934): the seat starts without them,
// and the caller is told so rather than finding out minutes into its work.
func hostToolsReport(ctx context.Context, servers []MCPServer) (json.RawMessage, map[string]string, map[string]string, []string) {
	var tools []hostJevonsTool
	routes := map[string]string{}
	originals := map[string]string{}
	// Every server at once (jevons 🎯T922): asked one after another, each
	// silent server cost its full 5 s timeout, and a seat with four of them
	// took ~25 s to launch — past the caller's 30 s tool deadline.
	// The launch waits at most hostToolsBudget (🎯T147): a stalled server
	// does not hold up the seat. Its fetch carries on in the background and
	// fills the cache, so the next launch has its tools.
	lists := make([][]hostJevonsTool, len(servers))
	var mu sync.Mutex
	done := make(chan int, len(servers))
	pending := map[int]string{}
	fetchCtx := context.WithoutCancel(ctx)
	for i, srv := range servers {
		if srv.URL == "" || (srv.Type != "" && srv.Type != "http") {
			continue // plugin-shaped / stdio: not registered
		}
		pending[i] = srv.Name
		go func(i int, name, url string) {
			l := listMCPToolsCached(fetchCtx, name, url)
			mu.Lock()
			lists[i] = l
			mu.Unlock()
			done <- i
		}(i, srv.Name, srv.URL)
	}
	budget := time.NewTimer(hostToolsBudget)
	defer budget.Stop()
wait:
	for len(pending) > 0 {
		select {
		case i := <-done:
			delete(pending, i)
		case <-budget.C:
			var late []string
			for _, name := range pending {
				late = append(late, name)
			}
			sort.Strings(late)
			slog.Warn("omp: host MCP servers did not list their tools in time; seat starts without them, and the next launch will have them if they answer",
				"servers", strings.Join(late, ","), "budget", hostToolsBudget)
			break wait
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// What the seat is offered, per server (🎯T147): the evidence that a
	// launch got every server's tools, or which it went without.
	var offered, unavailable []string
	for i, srv := range servers {
		if srv.URL == "" || (srv.Type != "" && srv.Type != "http") {
			continue
		}
		offered = append(offered, fmt.Sprintf("%s=%d", srv.Name, len(lists[i])))
		if len(lists[i]) == 0 {
			unavailable = append(unavailable, srv.Name)
		}
	}
	if len(offered) > 0 {
		slog.Info("omp: host tools offered to seat", "servers", strings.Join(offered, ","))
	}
	for i, srv := range servers {
		for _, t := range lists[i] {
			name := providerToolName(t.Name)
			if name == "" {
				continue
			}
			if _, dup := routes[name]; dup {
				continue
			}
			if name != t.Name {
				originals[name] = t.Name
				t.Name = name
			}
			tools = append(tools, t)
			routes[name] = srv.URL
		}
	}
	if len(tools) == 0 {
		return nil, nil, nil, unavailable
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return nil, nil, nil, unavailable
	}
	return raw, routes, originals, unavailable
}

// maxProviderToolName is the longest tool name every sidecar provider takes
// (OpenAI's limit; Anthropic allows 128).
const maxProviderToolName = 64

// providerToolName is name as the providers accept it (🎯T146): Anthropic
// refuses the whole request when one tool name falls outside
// ^[a-zA-Z0-9_-]{1,128}$, so jevons' `self_test.list` took down every turn
// of every sidecar seat. Other characters become '_'; a name with nothing
// left is not advertised.
func providerToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > maxProviderToolName {
		out = out[:maxProviderToolName]
	}
	if strings.Trim(out, "_") == "" {
		return ""
	}
	return out
}

// ompHistoryLost reads a sidecar's "ready": a launched seat whose sidecar
// did not restore its conversation starts without it (jevons 🎯T929). An
// adopted seat kept its conversation. A sidecar too old to report restoring
// never restored.
func ompHistoryLost(ev omp.Event) bool {
	return ev.How == "launched" && (ev.Restored == nil || !*ev.Restored)
}

// hostToolsTTL is how long a server's tool list is reused; a failed or empty
// answer is retried sooner (jevons 🎯T922).
const (
	hostToolsTTL     = 10 * time.Minute
	hostToolsFailTTL = time.Minute
	// hostToolsBudget is the longest a seat launch waits for host tool
	// lists; a healthy server answers well inside it (🎯T147).
	hostToolsBudget = 2 * time.Second
	// hostToolsDeadline bounds one tools/list, which may outlive the launch
	// that asked for it.
	hostToolsDeadline = 10 * time.Second
)

type hostToolsEntry struct {
	tools []hostJevonsTool
	at    time.Time
}

var hostToolsCache sync.Map // MCP URL -> hostToolsEntry

// listMCPToolsCached is listMCPTools, reused per server: tool lists rarely
// change, and a server that does not answer is not waited on again at every
// seat launch (jevons 🎯T922).
func listMCPToolsCached(ctx context.Context, name, mcpURL string) []hostJevonsTool {
	if tools, ok := cachedHostTools(mcpURL); ok {
		return tools
	}
	// Launches that need the same server at once share one fetch: one
	// request to the server, and one log line when it fails (🎯T147).
	call := &hostToolsCall{done: make(chan struct{})}
	if v, loaded := hostToolsInflight.LoadOrStore(mcpURL, call); loaded {
		shared := v.(*hostToolsCall)
		<-shared.done
		return shared.tools
	}
	defer func() {
		hostToolsInflight.Delete(mcpURL)
		close(call.done)
	}()
	// A fetch that finished between the check above and taking the slot
	// stored its answer before it let the slot go: use that, do not ask again.
	if tools, ok := cachedHostTools(mcpURL); ok {
		call.tools = tools
		return tools
	}
	tools, cause, err := listMCPTools(ctx, mcpURL)
	if cause != "" {
		slog.Warn("omp: host MCP server did not list its tools; seats start without them",
			"server", name, "url", mcpURL, "cause", cause, "err", err)
	}
	hostToolsCache.Store(mcpURL, hostToolsEntry{tools: tools, at: time.Now()})
	call.tools = tools
	return tools
}

// cachedHostTools is a server's cached tool list while it is fresh.
func cachedHostTools(mcpURL string) ([]hostJevonsTool, bool) {
	v, ok := hostToolsCache.Load(mcpURL)
	if !ok {
		return nil, false
	}
	e := v.(hostToolsEntry)
	ttl := hostToolsTTL
	if e.tools == nil {
		ttl = hostToolsFailTTL
	}
	if time.Since(e.at) >= ttl {
		return nil, false
	}
	return e.tools, true
}

// hostToolsCall is one tools/list fetch in progress, which other launches
// needing the same server wait on.
type hostToolsCall struct {
	done  chan struct{}
	tools []hostJevonsTool
}

var hostToolsInflight sync.Map // MCP URL -> *hostToolsCall

// Causes a host tools/list failure is logged under (🎯T147).
const (
	toolsCauseRefused   = "connect refused"
	toolsCauseTimeout   = "timeout"
	toolsCauseHTTP      = "http status"
	toolsCauseRPC       = "rpc error"
	toolsCauseReply     = "unreadable reply"
	toolsCauseTransport = "transport"
)

// listMCPTools asks one server for its tools, as an MCP client does: it
// opens a session with initialize, confirms it, then lists. A bare
// tools/list was dropped by every stdio server a seat had not initialized yet
// (bullseye, sawmill, spyder and vellum behind the broker's host, until a
// Claude seat happened to initialize them) and refused by HTTP servers that
// require a session (mnemo, atlassian) — 🎯T147. cause is empty on success,
// otherwise it names why the server gave no tools.
func listMCPTools(ctx context.Context, mcpURL string) ([]hostJevonsTool, string, error) {
	ctx, cancel := context.WithTimeout(ctx, hostToolsDeadline)
	defer cancel()
	session, err := mcpOpenSession(ctx, mcpURL)
	if err != nil {
		var se *mcpSessionError
		if errors.As(err, &se) {
			return nil, se.cause, err
		}
		return nil, toolsCauseTransport, err
	}
	if session != "" {
		// Close the session this listing opened; a server that keeps none
		// ignores it.
		defer mcpEndSession(mcpURL, session)
	}
	raw, _, cause, err := mcpPost(ctx, mcpURL, session, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`), "")
	if cause != "" {
		return nil, cause, fmt.Errorf("tools/list: %w", err)
	}
	if cause, err := mcpRPCError(raw); cause != "" {
		return nil, cause, fmt.Errorf("tools/list: %w", err)
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, toolsCauseReply, fmt.Errorf("tools/list: %w", err)
	}
	var out []hostJevonsTool
	for _, t := range envelope.Result.Tools {
		out = append(out, hostJevonsTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out, "", nil
}

// mcpSessionError is a failed session handshake, with its cause.
type mcpSessionError struct {
	cause string
	err   error
}

func (e *mcpSessionError) Error() string { return e.err.Error() }
func (e *mcpSessionError) Unwrap() error { return e.err }

// mcpOpenSession runs the MCP handshake — initialize, then
// notifications/initialized — and returns the session id the server
// assigned ("" for a server that keeps none).
func mcpOpenSession(ctx context.Context, mcpURL string) (string, error) {
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claudia-omp","version":"` + Version + `"}}}`)
	raw, session, cause, err := mcpPost(ctx, mcpURL, "", initialize, "")
	if cause != "" {
		return "", &mcpSessionError{cause, fmt.Errorf("initialize: %w", err)}
	}
	if cause, err := mcpRPCError(raw); cause != "" {
		return "", &mcpSessionError{cause, fmt.Errorf("initialize: %w", err)}
	}
	if _, _, cause, err := mcpPost(ctx, mcpURL, session, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), ""); cause != "" {
		if session != "" {
			mcpEndSession(mcpURL, session)
		}
		return "", &mcpSessionError{cause, fmt.Errorf("notifications/initialized: %w", err)}
	}
	return session, nil
}

// mcpPost sends one JSON-RPC message over streamable HTTP and returns the
// reply's JSON (the last SSE event when the server streams), the session id
// the server assigned, and a cause when it failed. A notification's reply
// may be empty.
func mcpPost(ctx context.Context, mcpURL, session string, body []byte, seat string) ([]byte, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", toolsCauseTransport, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if seat != "" {
		req.Header.Set(jevonsSeatHeader, seat)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			return nil, "", toolsCauseRefused, err
		case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
			return nil, "", toolsCauseTimeout, err
		}
		return nil, "", toolsCauseTransport, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			return nil, "", toolsCauseTimeout, err
		}
		return nil, "", toolsCauseTransport, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, "", toolsCauseHTTP, fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(raw[:min(len(raw), 200)]))
	}
	// A streamable-HTTP server may answer as SSE events; the reply is the last.
	if i := bytes.LastIndex(raw, []byte("\ndata:")); i >= 0 {
		raw = raw[i+len("\ndata:"):]
	} else if bytes.HasPrefix(raw, []byte("data:")) {
		raw = raw[len("data:"):]
	}
	return bytes.TrimSpace(raw), resp.Header.Get("Mcp-Session-Id"), "", nil
}

// mcpRPCError reports a JSON-RPC error reply, or a reply that is not JSON.
func mcpRPCError(raw []byte) (string, error) {
	var reply struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return toolsCauseReply, err
	}
	if reply.Error != nil {
		return toolsCauseRPC, fmt.Errorf("%d %s", reply.Error.Code, reply.Error.Message)
	}
	return "", nil
}

// mcpEndSession closes a streamable-HTTP session, best effort.
func mcpEndSession(mcpURL, session string) {
	ctx, cancel := context.WithTimeout(context.Background(), hostToolsBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, mcpURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", session)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}
