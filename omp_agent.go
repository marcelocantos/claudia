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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

var ErrSeatIdentityMismatch = errors.New("omp: loaded seat differs from registered provider or purpose")

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
	if err := conn.Send(omp.Message{
		Op:          op,
		Seat:        req.Config.Name,
		Provider:    provider,
		Model:       req.Config.Model,
		SummaryOnly: req.Config.SummaryOnly,
		Token:       token,
		Cwd:         req.Config.WorkDir,
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
	sessionID := req.Config.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	ctrl := &ompControl{
		conn: conn, bytes: make(chan []byte, 8),
		token: token, provider: provider,
		seat: req.Config.Name, model: req.Config.Model, cwd: req.Config.WorkDir,
		sessionID: sessionID, summaryOnly: req.Config.SummaryOnly,
	}
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
				return ctrl.send(omp.Message{
					Op:          omp.OpLoad,
					Seat:        req.Config.Name,
					Provider:    provider,
					Model:       model,
					SummaryOnly: req.Config.SummaryOnly,
					Token:       ctrl.token,
					Cwd:         req.Config.WorkDir,
				})
			},
			promptInFlight: func(*Agent) bool { return ctrl.inflight.Load() },
			stop: func(*Agent) {
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
		Cleanup: func() { conn.Close() },
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
	mu          sync.Mutex
	inflight    atomic.Bool
	refreshed   atomic.Bool
}

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
			result := runOMPTool(ev.Name, ev.CallID, ev.Text)
			_ = c.send(omp.Message{Op: omp.OpTool, CallID: ev.CallID, Result: result})
		case "turn_end", "error":
			c.inflight.Store(false)
			rejected := oauthRejected(ev.Text, ev.Snapshot)
			if rejected && c.refreshed.CompareAndSwap(false, true) {
				c.refreshRejectedToken()
			}
			a.publishEvent(Event{
				Type:       "assistant",
				Text:       ev.Text,
				IsError:    rejected || ev.Type == "error",
				StopReason: "end_turn",
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
		return
	}
	if err := FlushOMPPlans(context.Background()); err != nil {
		slog.Warn("omp token refreshed; keychain flush failed", "provider", c.provider, "err", err)
	}
	c.mu.Lock()
	c.token = rec.AccessToken
	c.mu.Unlock()
	if err := c.send(omp.Message{
		Op: omp.OpLoad, Seat: c.seat, Provider: c.provider,
		Model: c.model, SummaryOnly: c.summaryOnly, Token: rec.AccessToken, Cwd: c.cwd,
	}); err != nil {
		slog.Warn("omp token refreshed; sidecar reload failed", "seat", c.seat, "err", err)
	}
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
	}
	path := omp.ProductBrokerPath()
	seal := path != ""
	if path == "" {
		path = os.Args[0]
	}
	return omp.Store{BrokerPath: path, Run: run, RunStdin: runStdin, SealPath: seal}
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
	return omp.RecoverPlan(ctx, planStore(), login, provider)
}

// SetOMPToolExec installs the jevons_* callback the sidecar invokes
// (🎯T865). Production brokers set this to an HTTP tools/call against
// the live jevonsmcp URL.
func SetOMPToolExec(fn func(name, callID, args string) string) {
	ompToolExec = fn
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
// tools/call against the daemon's MCP surface.
func CallJevonsMCP(mcpURL, name, args string) string {
	if !strings.HasPrefix(name, "jevons_") {
		return fmt.Sprintf("omp: refusing non-jevons tool %q", name)
	}
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
