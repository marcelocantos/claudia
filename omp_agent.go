// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
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

var ErrSeatIdentityMismatch = errors.New("omp: loaded seat differs from registered provider or purpose")
var ErrNoSubscriptionModel = errors.New("omp: no provider-local subscription session model")

// ErrContextOverflow is what [Agent.WaitForResponse] wraps when the provider
// refused the turn as longer than the model's context window and the seat's
// own compaction could not bring it back under (🎯T148). It is terminal:
// sending the seat the same or another prompt cannot succeed, so a caller
// must not retry it as it would a transient failure. Jevons retried one
// every 30s for hours, each retry growing the prompt (jevons 🎯T926).
var ErrContextOverflow = errors.New("context overflow: the conversation is longer than the model's window")

// ReasonContextOverflow is [Event.Reason] on the error event of such a turn.
const ReasonContextOverflow = "context_overflow"

// useOMP selects the sidecar. The four subscription plans and the
// fleet ids grok / claude / codex / cursor all go through it (🎯T866.5).
// Config.OMP is no longer required for cursor.

func useOMP(cfg Config) bool {
	return ompProviderID(cfg.Provider) != ""
}

func ompProviderID(p Provider) string {
	switch p {
	case Provider(omp.Anthropic):
		return omp.Anthropic
	case Provider(omp.OpenAICodex):
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

// withDefaultOMPModel resolves an omitted model before either a broker grant
// or a direct start records the seat. An adopt reports the broker's persisted
// model instead of choosing a potentially different current default.
func withDefaultOMPModel(ctx context.Context, cfg Config) (Config, error) {
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

func providerLocalSessionModel(ctx context.Context, provider Provider) (string, error) {
	model, err := migrationSummaryModel(ctx, PlanProvider(provider))
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
	// Adopting a seat that is already running must not mint one, and must
	// not start a sidecar in order to discover that it is not there (🎯T869).
	if req.Config.RequireResume && !req.Config.AdoptOnly {
		if !omp.SeatHasHistory(omp.SpoolDir(), req.Config.Name) {
			return nil, fmt.Errorf("session %s: existing conversation required but no spool records for seat %q under %s — refusing to mint a replacement session",
				req.Config.SessionID, req.Config.Name, omp.SpoolDir())
		}
	}
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
	token, err := store.Ensure(req.Context, provider, login)
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
	if !req.Config.SummaryOnly {
		tools, toolRoutes, toolNames = hostToolsNamed(req.Context, req.Config.MCPServers)
	}
	// The session names the seat's conversation. The sidecar keeps it on
	// disk under this id, so a sidecar restart resumes it and a new session
	// starts fresh (🎯T151).
	sessionID := req.Config.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	if err := conn.Send(omp.Message{
		Op:          op,
		Seat:        req.Config.Name,
		Provider:    provider,
		Model:       req.Config.Model,
		SummaryOnly: req.Config.SummaryOnly,
		Token:       token,
		Cwd:         req.Config.WorkDir,
		SessionID:   sessionID,
		Tools:       tools,
	}); err != nil {
		conn.Close()
		return nil, err
	}
	if req.Config.AdoptOnly {
		// An older sidecar does not answer "adopt". Do not hold the resume.
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
	if ev.Type != "ready" {
		conn.Close()
		if ev.Reason == "seat_identity_mismatch" {
			return nil, fmt.Errorf("%w: %s", ErrSeatIdentityMismatch, ev.Text)
		}
		if req.Config.AdoptOnly {
			return nil, fmt.Errorf("%w: %s", ErrNoSessionWindow, req.Config.Name)
		}
		if ev.Text != "" {
			return nil, fmt.Errorf("omp: sidecar said %q, want ready: %s", ev.Type, ev.Text)
		}
		return nil, fmt.Errorf("omp: sidecar said %q, want ready", ev.Type)
	}
	ctrl := &ompControl{
		conn: conn, bytes: make(chan []byte, 8),
		token: token, provider: provider,
		seat: req.Config.Name, model: req.Config.Model, cwd: req.Config.WorkDir,
		sessionID: sessionID, summaryOnly: req.Config.SummaryOnly,
		tools: tools, toolServers: toolRoutes, toolNames: toolNames,
	}
	ompSeats.Store(ctrl, struct{}{})
	return &agentStart{
		Control:   ctrl,
		SessionID: sessionID,
		Ops: agentOps{
			send: func(a *Agent, text string) error {
				ctrl.inflight.Store(true)
				return ctrl.send(promptMessage(omp.OpPrompt, req.Config.Name, text, a, ctrl))
			},
			steer: func(a *Agent, text string) (DeliveryOutcome, error) {
				err := ctrl.send(promptMessage(omp.OpSteer, req.Config.Name, text, a, ctrl))
				return DeliveryOutcome{}, err
			},
			interrupt: func(*Agent) error {
				return ctrl.send(omp.Message{Op: omp.OpAbort, Seat: req.Config.Name})
			},
			setModel: func(_ *Agent, model string) error {
				ctrl.mu.Lock()
				ctrl.model = model
				ctrl.mu.Unlock()
				return ctrl.send(ctrl.loadMessage())
			},
			promptInFlight: func(*Agent) bool { return ctrl.inflight.Load() },
			stop: func(*Agent) {
				ompSeats.Delete(ctrl)
				if req.Config.SummaryOnly {
					_ = ctrl.send(omp.Message{Op: omp.OpDrop, Seat: req.Config.Name})
				}
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

type ompControl struct {
	conn        *omp.Conn
	bytes       chan []byte
	token       string
	provider    string
	seat        string
	model       string
	cwd         string
	sessionID   string
	summaryOnly bool
	// tools is the host tool list the seat was loaded with; a reload keeps it.
	tools json.RawMessage
	// toolServers routes a model tool name to the AgentDef.MCPServers URL
	// that declared it (🎯T871.1). A name absent here (jevons_* offered via
	// resolveFallbackTool, or a legacy caller) falls back to runOMPTool.
	toolServers map[string]string
	// toolNames maps an advertised tool name to the server's own name for
	// it, where the provider needed it renamed (🎯T146).
	toolNames map[string]string
	mu        sync.Mutex
	inflight  atomic.Bool
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

func (c *ompControl) pump(a *Agent) {
	defer close(c.bytes)
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
		switch ev.Type {
		case "accepted":
			// The sidecar took the prompt (a turn began, or it was queued
			// behind the running one). Before the first token this is the
			// only sign the prompt landed. A turn is open from here: that
			// includes the turn a hard-stop starts to deliver what was
			// queued (🎯T138), which no send of ours opened.
			c.inflight.Store(true)
			a.publishEvent(Event{Type: "progress", ProgressType: ProgressPromptAccepted})
		case "absorbed":
			// The model took a message queued or steered behind the turn;
			// an escalation for it stops here (🎯T138).
			a.publishEvent(Event{Type: "progress", ProgressType: ProgressDeliveryAbsorbed, Text: ev.Text})
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
			a.publishEvent(Event{
				Type:          "assistant",
				Text:          visible,
				PreviewUpdate: PreviewUpdateAppend,
			})
		case "tool_call":
			if c.summaryOnly {
				_ = c.send(omp.Message{Op: omp.OpTool, CallID: ev.CallID, Result: "tools are unavailable to a migration summarizer"})
				continue
			}
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
		case "turn_end", "error":
			c.inflight.Store(false)
			rejected := oauthRejected(ev.Text, ev.Snapshot)
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
			a.publishEvent(Event{
				Type:       "assistant",
				Text:       text,
				IsError:    rejected || ev.Type == "error" || refusal != "",
				StopReason: "end_turn",
				Reason:     reason,
			})
		}
	}
}

// oauthRejected reports a provider refusal of the access token, on the
// error event or inside the turn snapshot. A bad refresh token for a
// different plan is not this signal (🎯T868).
func oauthRejected(text string, snapshot json.RawMessage) bool {
	blob := text
	if len(snapshot) > 0 {
		blob += "\n" + string(snapshot)
	}
	if blob == "" {
		return false
	}
	for _, n := range []string{
		"could not be validated",
		"unauthenticated:bad-credentials",
		"invalid_token",
		"authentication_error",
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
	if tok, err := planStore().AccessToken(context.Background(), c.provider); err == nil && tok != c.currentToken() {
		slog.Info("omp token rejected; seat takes the plan's newer token", "provider", c.provider, "seat", c.seat)
		c.reload(tok)
		return
	}
	c.mu.Lock()
	if !c.lastRefresh.IsZero() && time.Since(c.lastRefresh) < ompRefreshBackoff {
		c.mu.Unlock()
		return
	}
	c.lastRefresh = time.Now()
	c.mu.Unlock()
	c.refreshRejectedToken()
}

func (c *ompControl) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// loadMessage is the load that (re)configures this seat with its current
// token, model and host tools.
func (c *ompControl) loadMessage() omp.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return omp.Message{
		Op: omp.OpLoad, Seat: c.seat, Provider: c.provider, SessionID: c.sessionID,
		Model: c.model, SummaryOnly: c.summaryOnly, Token: c.token, Cwd: c.cwd,
		Tools: c.tools,
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
// holding another one, and says how many it reloaded.
func reloadOMPSeats(ctx context.Context, provider string) int {
	tok, err := planStore().AccessToken(ctx, provider)
	if err != nil {
		return 0
	}
	n := 0
	ompSeats.Range(func(k, _ any) bool {
		c := k.(*ompControl)
		if c.provider == provider && c.currentToken() != tok {
			c.reload(tok)
			n++
		}
		return true
	})
	return n
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
	c.reload(rec.AccessToken)
}

func sidecarAuthScript() string {
	if p := os.Getenv("CLAUDIA_OMP_AUTH"); p != "" {
		return p
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "sidecar/auth.ts"
	}
	return filepath.Join(filepath.Dir(file), "sidecar", "auth.ts")
}

// ompToolExec is the Go callback for jevons_* tool calls. Tests replace it.
var ompToolExec func(name, callID, args string) string

// EnsureOMPSidecar starts the detached Bun sidecar if it is not already
// listening. A jevonsd or broker bounce must not call StopSidecar.
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
	return omp.LoginPlans(ctx, store, login, ids...)
}

// RecoverOMPAuth asks the running broker to repair a plan's authentication.
// The broker owns the in-memory Keychain copy; a separate client process must
// never attempt to update that copy itself.
func RecoverOMPAuth(ctx context.Context, provider Provider) error {
	id := ompProviderID(provider)
	if id == "" {
		return fmt.Errorf("omp: %s is not a subscription provider", provider)
	}
	client, err := dialBroker()
	if err != nil {
		return fmt.Errorf("omp: broker is required for reauthentication: %w", err)
	}
	defer client.Close()
	resp, err := client.call(ctx, &broker.Request{Type: broker.TypeAuthRecover,
		AuthRecover: &broker.NamedRequest{Name: id}})
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
// It retries a failed read, refreshes the named plan, and falls back to
// interactive login only for an invalid refresh grant.
func RecoverOMPPlan(ctx context.Context, provider string) error {
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
	if err := omp.RecoverPlan(ctx, planStore(), login, provider); err != nil {
		return err
	}
	// The seats already running still hold the rejected token (🎯T141).
	if n := reloadOMPSeats(ctx, provider); n > 0 {
		slog.Info("omp plan recovered; live seats reloaded", "provider", provider, "seats", n)
	}
	return nil
}

// SetOMPToolExec installs the jevons_* callback the sidecar invokes
// (🎯T865). Production brokers set this to an HTTP tools/call against
// the live jevonsmcp URL.
func SetOMPToolExec(fn func(name, callID, args string) string) {
	ompToolExec = fn
}

// runTool executes one model tool call for this seat. A tool whose home
// server was discovered from AgentDef.MCPServers (🎯T871.1) is routed there
// with a structured tools/call; anything else — in practice jevons_* —
// still goes through runOMPTool's jevons-only runner, the same boundary
// resolveFallbackTool enforces in the sidecar (🎯T864.3).
func (c *ompControl) runTool(name, callID, args string) string {
	if c != nil && c.toolServers != nil {
		if url, ok := c.toolServers[name]; ok {
			if orig, renamed := c.toolNames[name]; renamed {
				name = orig
			}
			return CallMCPTool(url, name, args)
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

// DefaultOMPToolExec POSTs a JSON-RPC tools/call to the jevons MCP
// endpoint. JEVONS_MCP_URL wins; otherwise the development :13705 path.
func DefaultOMPToolExec(name, callID, args string) string {
	url := strings.TrimSpace(os.Getenv("JEVONS_MCP_URL"))
	if url == "" {
		url = "http://127.0.0.1:13705/mcp"
	}
	return CallJevonsMCP(url, name, args)
}

// CallJevonsMCP is the production jevons_* runner: one HTTP JSON-RPC
// tools/call against the daemon's MCP surface. Anything not jevons_* is
// refused here — that prefix boundary is resolveFallbackTool's (🎯T864.3).
// A tool routed from AgentDef.MCPServers (🎯T871.1) calls [CallMCPTool]
// directly and is not subject to this refusal.
func CallJevonsMCP(mcpURL, name, args string) string {
	if !strings.HasPrefix(name, "jevons_") {
		return fmt.Sprintf("omp: refusing non-jevons tool %q", name)
	}
	return CallMCPTool(mcpURL, name, args)
}

// CallMCPTool POSTs a structured JSON-RPC tools/call to an MCP server
// (🎯T871.1). No name prefix is required: the caller already decided this
// tool belongs to this server, via [hostTools]'s discovered route.
func CallMCPTool(mcpURL, name, args string) string {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("omp: %s request: %v", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("omp: %s call failed: %v", name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Sprintf("omp: %s read: %v", name, err)
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
		return nil, nil, nil
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		return nil, nil, nil
	}
	return raw, routes, originals
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
	if v, ok := hostToolsCache.Load(mcpURL); ok {
		e := v.(hostToolsEntry)
		ttl := hostToolsTTL
		if e.tools == nil {
			ttl = hostToolsFailTTL
		}
		if time.Since(e.at) < ttl {
			return e.tools
		}
	}
	tools, cause, err := listMCPTools(ctx, mcpURL)
	if cause != "" {
		// One line per failure, naming the server and why (🎯T147).
		slog.Warn("omp: host MCP server did not list its tools; seats start without them",
			"server", name, "url", mcpURL, "cause", cause, "err", err)
	}
	hostToolsCache.Store(mcpURL, hostToolsEntry{tools: tools, at: time.Now()})
	return tools
}

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
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claudia-omp","version":"` + Version + `"}}}`)
	raw, session, cause, err := mcpPost(ctx, mcpURL, "", initialize)
	if cause != "" {
		return nil, cause, fmt.Errorf("initialize: %w", err)
	}
	if cause, err := mcpRPCError(raw); cause != "" {
		return nil, cause, fmt.Errorf("initialize: %w", err)
	}
	if session != "" {
		// Close the session this listing opened; a server that keeps none
		// ignores it.
		defer mcpEndSession(mcpURL, session)
	}
	if _, _, cause, err := mcpPost(ctx, mcpURL, session, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); cause != "" {
		return nil, cause, fmt.Errorf("notifications/initialized: %w", err)
	}
	raw, _, cause, err = mcpPost(ctx, mcpURL, session, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
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

// mcpPost sends one JSON-RPC message over streamable HTTP and returns the
// reply's JSON (the last SSE event when the server streams), the session id
// the server assigned, and a cause when it failed. A notification's reply
// may be empty.
func mcpPost(ctx context.Context, mcpURL, session string, body []byte) ([]byte, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", toolsCauseTransport, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
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
