// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrCursorResumeDenied is returned when session/load failed for a
// conversation that already has store.db (or RequireResume). Callers
// must not retry Launch — a second client stacks a writer on the same
// store (🎯T541.1).
var ErrCursorResumeDenied = errors.New("existing conversation; refusing to mint a replacement session")

// ErrCursorPromptStuck is returned when a session/prompt was written to a
// live ACP peer that then said nothing at all about it — no chunk, no
// thought, no tool call, no result — for cursorPromptSilenceBound, twice
// over (🎯T83). It is deliberately NOT ErrTurnInFlight: a stuck mint that
// reports itself as busy is the exact misreading that made a consumer
// stop, park, start, kill and remint a seat four times in four minutes to
// clear one held brief. The seat is left idle, so a retry can use it.
var ErrCursorPromptStuck = errors.New("cursor acp: session/prompt accepted no work")

// IsCursorResumeDenied reports whether err is (or wraps) ErrCursorResumeDenied.
// A daemon grant failure arrives as a ProtocolError whose Msg copies the
// sentinel; errors.Is cannot see through that, so the text is also matched.
func IsCursorResumeDenied(err error) bool {
	if errors.Is(err, ErrCursorResumeDenied) {
		return true
	}
	return err != nil && strings.Contains(err.Error(), ErrCursorResumeDenied.Error())
}

// cursorACPClient is a minimal ACP client over JSON-RPC 2.0 stdio to
// `agent acp`. See https://cursor.com/docs/cli/acp and
// https://agentclientprotocol.com.
type cursorACPClient struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	ownsProcess bool

	mu        sync.Mutex
	nextID    int64
	pending   map[int64]chan acpRPCMessage
	closed    bool
	writeMu   sync.Mutex
	closeOnce sync.Once

	// peerSeq counts inbound messages from the peer. A prompt watches it
	// for movement: any movement at all is proof the peer is engaging
	// with the session, which is the only signal that separates a turn
	// that started slowly from one that never started (🎯T83).
	peerSeq   uint64
	peerWoke  chan struct{}
	firstDone bool

	sessionID string
	onEvent   func(Event)
	onClose   func()
	// prompts is the open turn's session/prompt id stack (🎯T72.1).
	prompts acpPromptStack

	// afterReissueCancel runs in promptWatchingForSilence between the
	// session/cancel write and the re-delivery write. Nil in production.
	// It exists so a test can land the peer's answer in exactly that
	// window, which is where 🎯T92's second-wait snapshot used to be taken
	// after it — the ordering that made a redeemed answer count as
	// silence. Without the seam the window is a race the host decides
	// (🎯T107); with it the differential is deterministic.
	afterReissueCancel func()
}

// cursorACPArgs builds the argv after the agent binary. Root flags must
// precede the `acp` mode word — a --model after "acp" is ignored.
// Auth uses the real agent login Keychain, or CURSOR_API_KEY in the
// process environment (Cursor reads it without a hermetic HOME).
func cursorACPArgs(model string) []string {
	args := []string{"--force", "--trust", "--approve-mcps"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, "acp")
}

// Saved sessions with a full MCP map can take minutes to load. This bound
// allows that cold start; explicit cancellation remains immediate.
const cursorACPStartupTimeout = 5 * time.Minute

// cursorPromptSilenceBound is how long the first prompt of a session may
// go without the peer saying ANYTHING about it before the delivery is
// treated as stuck.
//
// It bounds silence before the first inbound message of the turn, not the
// turn itself: the very first thing the peer sends disarms it, so a long
// healthy turn is never touched. That distinction is what makes a bound
// safe here at all.
//
// The number is measured, not chosen. Three real cursor-agent mints, each
// timed from session/prompt to the first inbound message for the session:
// 14.0s, 14.1s, 18.3s — and the first thing to arrive is an
// agent_thought_chunk, not reply text. A bound in the seconds anyone would
// reach for would call every healthy mint stuck. This is ~6.5x the worst
// observed. Recovery is cheap and a false positive is not, so the
// generous side is the correct side to err on.
//
// It is a var only so hermetics can shorten it; nothing in production
// writes it.
var cursorPromptSilenceBound = 120 * time.Second

func startCursorACP(ctx context.Context, bin string, workDir, model, sessionID string, requireResume bool, mcpServers []any, extraEnv []string, onEvent func(Event), onClose func()) (*cursorACPClient, error) {
	ctx, cancel := context.WithTimeout(ctx, cursorACPStartupTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, cursorACPArgs(model)...)
	cmd.Dir = workDir
	if len(extraEnv) > 0 {
		cmd.Env = appendEnv(nil, extraEnv)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("cursor acp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("cursor acp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("cursor acp stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start cursor acp: %w", err)
	}

	c := &cursorACPClient{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		ownsProcess: true,
		pending:     make(map[int64]chan acpRPCMessage),
		peerWoke:    make(chan struct{}, 1),
		onEvent:     onEvent,
		onClose:     onClose,
		sessionID:   sessionID,
	}

	go c.drainStderr()
	go c.readLoop()

	// Closing the transport does not acquire its write lock, so cancellation
	// also interrupts a child that has stopped reading stdin.
	closed := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		c.Close()
		close(closed)
	})
	defer func() {
		if stopCancel != nil && !stopCancel() {
			<-closed
		}
	}()
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.authenticate(ctx); err != nil {
		c.Close()
		return nil, err
	}
	if err := c.openSession(ctx, workDir, sessionID, requireResume, mcpServers); err != nil {
		c.Close()
		return nil, err
	}
	// If cancellation won the handoff race, no closed client escapes. Once
	// disarmed, a later cancellation cannot kill the successfully started agent.
	if !stopCancel() {
		<-closed
		return nil, ctx.Err()
	}
	stopCancel = nil
	if err := ctx.Err(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *cursorACPClient) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *cursorACPClient) drainStderr() {
	if c.stderr == nil {
		return
	}
	sc := bufio.NewScanner(c.stderr)
	sc.Buffer(make([]byte, 256*1024), 256*1024)
	for sc.Scan() {
		slog.Debug("cursor acp stderr", "line", sc.Text())
	}
}

func (c *cursorACPClient) readLoop() {
	defer func() {
		c.mu.Lock()
		c.closed = true
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		c.wakePromptWaiters()
		if c.onClose != nil {
			c.onClose()
		}
	}()
	if c.stdout == nil {
		return
	}
	sc := newACPLineScanner(c.stdout)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		c.dispatchMessage(line)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if !closed {
		logACPScanErr("cursor", sc.Err())
	}
}

// notePeerActivity records that the peer said something. Every inbound
// line counts, including one this client cannot parse: the question a
// stuck prompt asks is whether the peer is engaging at all, not whether
// it is engaging in a shape we understand (🎯T83).
func (c *cursorACPClient) notePeerActivity() {
	c.mu.Lock()
	c.peerSeq++
	ch := c.peerWoke
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// wakePromptWaiters releases an awaitPeerActivity that is parked on a
// transport which has just died, so a stuck-prompt wait ends with the
// connection rather than sitting out the full bound.
func (c *cursorACPClient) wakePromptWaiters() {
	c.mu.Lock()
	ch := c.peerWoke
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (c *cursorACPClient) dispatchMessage(line []byte) {
	c.notePeerActivity()
	var msg acpRPCMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		slog.Debug("cursor acp ignore non-json line", "err", err)
		return
	}
	if msg.ID != nil && msg.Method != "" {
		c.handleServerRequest(msg)
		return
	}
	if msg.ID != nil {
		answered := acpResultAnswersTurn(msg)
		c.mu.Lock()
		ch := c.pending[*msg.ID]
		if ch != nil {
			delete(c.pending, *msg.ID)
		}
		// Read the surviving id BEFORE settling: a redeemed delivery's
		// terminal event belongs to the id the turn's chunks went out
		// under, not to the delivery this client had stopped waiting for.
		surviving := c.prompts.top()
		settle := c.prompts.settle(*msg.ID, answered)
		sessionID := c.sessionID
		c.mu.Unlock()
		switch settle {
		case acpPromptTurnDone:
			c.publishPromptResult(msg, sessionID, strconv.FormatInt(*msg.ID, 10))
		case acpPromptRedeemed:
			// The peer was slow, not deaf: it answered the delivery the
			// silence watch gave up on. That answer is the turn's, and
			// ends it — dropping it left the caller with no terminal
			// event to wait for at all (🎯T92).
			slog.Warn("cursor acp abandoned delivery answered after the re-issue; ending the turn on it",
				"session", sessionID, "prompt", *msg.ID, "turn", surviving)
			c.publishPromptResult(msg, sessionID, strconv.FormatInt(surviving, 10))
		case acpPromptSuperseded:
			// A steered-over prompt answered early (Cursor: cancelled).
			// The turn continues on the top id; no terminal event.
			publishEvent(c.onEvent, acpPromptSupersededEvent(sessionID, *msg.ID, msg))
		}
		if ch != nil {
			select {
			case ch <- msg:
			default:
			}
		}
		return
	}
	if msg.Method != "" {
		c.handleNotification(msg)
	}
}

func (c *cursorACPClient) handleServerRequest(msg acpRPCMessage) {
	switch msg.Method {
	case "session/request_permission":
		c.mu.Lock()
		sid, promptID := c.sessionID, c.prompts.top()
		c.mu.Unlock()
		publishEvent(c.onEvent, acpPermissionEvent(sid, promptID, msg.Params))
		reply := permissionSelectedReply(msg.Params)
		if permissionMutatesBullseye(msg.Params) {
			slog.Warn("cursor acp refused ledger mutation",
				"reason", LedgerRefuseReason)
		}
		_ = c.reply(msg.ID, reply)
	case "cursor/ask_question":
		// Unattended: skip rather than stall the turn.
		_ = c.reply(msg.ID, map[string]any{
			"outcome": map[string]any{"outcome": "skipped", "reason": "claudia unattended client"},
		})
	case "cursor/create_plan":
		_ = c.reply(msg.ID, map[string]any{
			"outcome": map[string]any{"outcome": "accepted"},
		})
	case "fs/read_text_file", "fs/write_text_file",
		"terminal/create", "terminal/output", "terminal/release",
		"terminal/wait_for_exit", "terminal/kill":
		_ = c.replyError(msg.ID, -32601, "claudia cursor acp client does not implement "+msg.Method)
	default:
		slog.Warn("cursor acp unhandled server request", "method", msg.Method, "params", string(msg.Params))
		_ = c.replyError(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

func (c *cursorACPClient) handleNotification(msg acpRPCMessage) {
	switch msg.Method {
	case "session/update":
		c.handleSessionUpdate(msg.Params)
	case "cursor/update_todos", "cursor/task", "cursor/generate_image":
		if c.onEvent != nil {
			c.onEvent(Event{Type: "progress", Raw: msg.Params, ProgressType: msg.Method})
		}
	}
}

func (c *cursorACPClient) handleSessionUpdate(params json.RawMessage) {
	if c.onEvent == nil || len(params) == 0 {
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Title  string `json:"title"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
		} `json:"update"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	c.mu.Lock()
	promptID := c.prompts.top()
	clientSessionID := c.sessionID
	c.mu.Unlock()
	sessionID := clientSessionID
	if p.SessionID != "" {
		sessionID = p.SessionID
	}
	turnID := ""
	if promptID != 0 && (p.SessionID == "" || clientSessionID == "" || p.SessionID == clientSessionID) {
		turnID = strconv.FormatInt(promptID, 10)
	}
	if ev, ok := acpProgressEvent(sessionID, turnID, params); ok {
		c.onEvent(ev)
		return
	}
	probe, _ := parseACPUpdate(params)
	usage := acpUsage(probe)
	switch p.Update.SessionUpdate {
	case "agent_message_chunk":
		text := ""
		if p.Update.Content != nil {
			text = p.Update.Content.Text
		}
		if text == "" {
			return
		}
		c.onEvent(Event{Type: "assistant", SessionID: sessionID, TurnID: turnID, Raw: params, Text: text, Usage: usage, PreviewUpdate: PreviewUpdateAppend})
	case "user_message_chunk":
		text := ""
		if p.Update.Content != nil {
			text = p.Update.Content.Text
		}
		c.onEvent(Event{Type: "user", SessionID: sessionID, TurnID: turnID, Raw: params, Text: text, Usage: usage})
	}
}

func (c *cursorACPClient) initialize(ctx context.Context) error {
	_, err := c.requestContext(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "claudia",
			"version": Version,
		},
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	})
	if err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	_ = c.notify("notifications/initialized", map[string]any{})
	return nil
}

func (c *cursorACPClient) authenticate(ctx context.Context) error {
	_, err := c.requestContext(ctx, "authenticate", map[string]any{
		"methodId": "cursor_login",
	})
	if err != nil {
		return fmt.Errorf("acp authenticate cursor_login: %w", err)
	}
	return nil
}

func (c *cursorACPClient) openSession(ctx context.Context, workDir, preferSessionID string, requireResume bool, mcpServers []any) error {
	if mcpServers == nil {
		mcpServers = []any{}
	}
	if preferSessionID != "" {
		err := c.loadSession(ctx, preferSessionID, workDir, mcpServers)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("acp session/load %s interrupted; refusing replacement: %w", preferSessionID, err)
		}
		// A store.db means this id already hosted a conversation.
		// session/new after a failed load stacks a second writer and
		// often dies with "client closed" (🎯T541.1).
		if requireResume || cursorACPStoreExists(preferSessionID) {
			return fmt.Errorf("acp session/load %s: %w (%w)", preferSessionID, err, ErrCursorResumeDenied)
		}
		slog.Warn("cursor acp session/load failed for unmaterialized id; creating new session", "err", err, "session", preferSessionID)
	}
	return c.createSession(ctx, workDir, mcpServers)
}

func (c *cursorACPClient) createSession(ctx context.Context, workDir string, mcpServers []any) error {
	if mcpServers == nil {
		mcpServers = []any{}
	}
	result, err := c.requestContext(ctx, "session/new", map[string]any{
		"cwd":        workDir,
		"mcpServers": mcpServers,
	})
	if err != nil {
		return fmt.Errorf("acp session/new: %w", err)
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		return fmt.Errorf("acp session/new decode: %w", err)
	}
	if out.SessionID == "" {
		return fmt.Errorf("acp session/new: empty sessionId")
	}
	c.mu.Lock()
	c.sessionID = out.SessionID
	c.mu.Unlock()
	return nil
}

func (c *cursorACPClient) loadSession(ctx context.Context, sessionID, workDir string, mcpServers []any) error {
	if mcpServers == nil {
		mcpServers = []any{}
	}
	result, err := c.requestContext(ctx, "session/load", map[string]any{
		"sessionId":  sessionID,
		"cwd":        workDir,
		"mcpServers": mcpServers,
	})
	if err != nil {
		return err
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(result, &out)
	c.mu.Lock()
	if out.SessionID != "" {
		c.sessionID = out.SessionID
	} else {
		c.sessionID = sessionID
	}
	c.mu.Unlock()
	return nil
}

func (c *cursorACPClient) Prompt(text string) error {
	c.mu.Lock()
	sid := c.sessionID
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("cursor acp: client closed")
	}
	if sid == "" {
		c.mu.Unlock()
		return fmt.Errorf("cursor acp: no session")
	}
	if c.prompts.inFlight() {
		c.mu.Unlock()
		return fmt.Errorf("cursor acp: %w", ErrTurnInFlight)
	}
	// Only the session's first prompt is watched. That is where the seat
	// wedges (🎯T83): session/new answers, the mint looks healthy, and the
	// opening brief falls into a peer that never speaks. Once a seat has
	// demonstrably taken work, a plain write is the right thing again, and
	// Send stays non-blocking for the rest of the seat's life.
	//
	// The watch also needs something to watch (🎯T91). peerSeq only moves
	// from readLoop, and readLoop only exists on a client that owns a
	// transport — the same constructor that installs peerWoke. On a client
	// assembled without one, no message can ever arrive during the call, so
	// the wait is not a wait: its verdict is fixed before it starts, and it
	// would spend both deliveries' bounds to report a healthy peer stuck.
	// Arm the watch only where the peer can disarm it.
	watch := !c.firstDone && c.peerWoke != nil
	unwatchable := !c.firstDone && c.peerWoke == nil
	id := atomic.AddInt64(&c.nextID, 1)
	c.prompts.push(id)
	c.mu.Unlock()
	publishEvent(c.onEvent, acpPromptAcceptedEvent(sid, id))

	if !watch {
		if unwatchable {
			// Loud, because on a transport-owning client this would be a
			// silent loss of the 🎯T83 guarantee rather than a stub.
			slog.Warn("cursor acp opening prompt not watched for silence: client has no peer wake path",
				"session", sid, "prompt", id, "owns_process", c.ownsProcess)
		}
		return c.write(acpPromptRequest(id, sid, text))
	}
	return c.promptWatchingForSilence(sid, id, text)
}

// promptWatchingForSilence writes the session's opening prompt and waits
// for the peer to say anything at all about it. A peer that answers — with
// a thought, a chunk, a tool call, a result, anything — has taken the work,
// and this returns as soon as that lands.
//
// A peer that says nothing gets the brief once more on a re-established
// turn, because the observed failure is a delivery that vanished rather
// than an agent that refused. If the re-issue is met with the same silence,
// the seat is left IDLE and the caller is told so: a consumer that has a
// typed error and a usable seat can retry in place, which is what
// distinguishes this from the stop/park/start/kill/remint cycle the bug
// forced.
func (c *cursorACPClient) promptWatchingForSilence(sid string, id int64, text string) error {
	// Read the counter BEFORE the write. A peer can answer between the
	// write returning and the wait starting — in hermetics it usually
	// does — and a snapshot taken afterwards would miss that reply and
	// sit out the whole bound on a seat that is working fine.
	seq := c.peerSeqNow()
	if err := c.write(acpPromptRequest(id, sid, text)); err != nil {
		c.mu.Lock()
		c.prompts.clear()
		c.mu.Unlock()
		return err
	}
	first := c.awaitPeerActivity(seq, cursorPromptSilenceBound)
	if first.spoke {
		c.mu.Lock()
		c.firstDone = true
		c.mu.Unlock()
		return nil
	}

	// Abandoning this delivery and swapping in its replacement is ONE
	// critical section, and it happens before anything else goes on the
	// wire (🎯T92). Two races close here.
	//
	// A reply that lands in the instant the bound just closed is seen
	// immediately below, and there is then nothing to re-establish: the
	// peer answered, late, and the caller gets its answer.
	//
	// A reply that lands any later arrives to find its id still known to
	// the turn and still able to end it. The old code cleared the stack
	// first, so that reply settled as acpPromptNotOurs and published
	// nothing: the answer was on the wire and the turn never ended.
	//
	// Cancelling before the swap would reopen both windows, and add a
	// third — the cancel's own result would arrive while the abandoned
	// id was still the top of the stack, and end the turn with nothing
	// in it.
	c.mu.Lock()
	if c.peerSeq > seq {
		c.firstDone = true
		c.mu.Unlock()
		slog.Warn("cursor acp opening prompt spoke as its bound expired; keeping the turn",
			"session", sid, "prompt", id, "bound", cursorPromptSilenceBound)
		return nil
	}
	closed := c.closed
	retryID := atomic.AddInt64(&c.nextID, 1)
	c.prompts.reissue(retryID)
	// The second wait's snapshot is taken HERE, in the same critical
	// section as the swap, not after the cancel goes out. Anything the
	// peer says from this instant on is an answer to the re-established
	// turn — including the late answer to the abandoned delivery, which
	// the swap has just made redeemable. Snapshotting after the cancel
	// left a window in which that answer redeemed the turn and published
	// its terminal event, yet counted as silence: the second wait then
	// expired and the caller was told ErrCursorPromptStuck for a turn
	// that had already been answered, so it never waited for the reply
	// (🎯T92, caught by TestCursorReissueKeepsAReplyRacingTheRedelivery
	// in gate ba76b76a).
	seq = c.peerSeq
	c.mu.Unlock()
	if closed {
		c.mu.Lock()
		c.prompts.clear()
		c.mu.Unlock()
		return fmt.Errorf("cursor acp: client closed")
	}

	slog.Warn("cursor acp opening prompt drew no response; re-establishing the turn",
		"session", sid, "prompt", id, "retry", retryID,
		"bound", cursorPromptSilenceBound, "observed", first.String())

	// Drop the vanished turn on the peer's side too, so the retry is a
	// fresh prompt rather than a second one stacked on a turn the peer
	// may still believe is open.
	_ = c.notify("session/cancel", map[string]any{"sessionId": sid})
	if c.afterReissueCancel != nil {
		c.afterReissueCancel()
	}
	publishEvent(c.onEvent, acpPromptAcceptedEvent(sid, retryID))
	if err := c.write(acpPromptRequest(retryID, sid, text)); err != nil {
		c.mu.Lock()
		c.prompts.clear()
		c.mu.Unlock()
		return err
	}
	second := c.awaitPeerActivity(seq, cursorPromptSilenceBound)
	if second.spoke {
		c.mu.Lock()
		c.firstDone = true
		c.mu.Unlock()
		return nil
	}

	// Leave the seat idle. An in-flight stack here would report the dead
	// turn as ErrTurnInFlight to every later Send — the pin this target
	// exists to remove.
	c.mu.Lock()
	c.prompts.clear()
	c.mu.Unlock()
	// Name what was waited for, not just how long (🎯T91). Whoever reads
	// this next needs to know which deliveries went out, what each wait
	// actually observed, and whether the transport was still alive — a
	// bare duration sends them back to the wire to find out.
	return fmt.Errorf("%w: session %s took two deliveries (prompt %d: %s; prompt %d: %s) with a %v bound on each",
		ErrCursorPromptStuck, sid, id, first, retryID, second, cursorPromptSilenceBound)
}

// peerSeqNow samples the inbound-message counter.
func (c *cursorACPClient) peerSeqNow() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peerSeq
}

// peerWaitOutcome is what a silence wait observed. A bare "the peer did
// not speak" is the wrong thing to report from here (🎯T91): silence, a
// transport that died under the wait, and a wait that could never have
// been woken at all are three different faults with three different
// repairs, and the bound alone cannot tell them apart.
type peerWaitOutcome struct {
	spoke    bool          // the peer said something after the snapshot
	closed   bool          // the transport died while parked
	observed uint64        // inbound messages seen during the wait
	waited   time.Duration // wall time actually spent parked
}

// String renders the outcome for a log line or an error, naming what the
// wait was watching rather than only how long it watched.
func (o peerWaitOutcome) String() string {
	switch {
	case o.spoke:
		return fmt.Sprintf("peer spoke after %v (%d inbound)", o.waited.Round(time.Millisecond), o.observed)
	case o.closed:
		return fmt.Sprintf("transport closed after %v with no inbound message", o.waited.Round(time.Millisecond))
	default:
		return fmt.Sprintf("no inbound message of any kind in %v", o.waited.Round(time.Millisecond))
	}
}

// awaitPeerActivity waits for the peer to send anything after the counter
// read start, up to d, and reports what it saw.
//
// Two things end it early. A closed transport: a dead peer is not a silent
// one, and the caller's own error path beats burning the bound. And a
// client with no wake channel: nothing can ever move peerSeq on such a
// client, so parking on it is a sleep with a predetermined verdict, not a
// wait. Callers arm the watch only when the wake path exists (see Prompt),
// and this is the second line of that defence.
func (c *cursorACPClient) awaitPeerActivity(start uint64, d time.Duration) peerWaitOutcome {
	c.mu.Lock()
	ch := c.peerWoke
	c.mu.Unlock()

	began := time.Now()
	report := func() peerWaitOutcome {
		c.mu.Lock()
		seq, closed := c.peerSeq, c.closed
		c.mu.Unlock()
		return peerWaitOutcome{
			spoke:    seq > start,
			closed:   closed,
			observed: seq - start,
			waited:   time.Since(began),
		}
	}
	if ch == nil {
		return report()
	}

	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		if out := report(); out.spoke || out.closed {
			return out
		}
		select {
		case <-ch:
		case <-deadline.C:
			return report()
		}
	}
}

// Steer folds text into the running turn by writing a second
// session/prompt whose id supersedes the one in flight (🎯T72.1). No
// session/cancel is sent: Cursor breaks into the running turn at its next breakpoint. The
// superseded prompt's own JSON-RPC result, when it arrives, does not end
// the turn; the new top id does. On an idle session Steer is a plain
// Prompt and reports acpSubmitMechanism.
func (c *cursorACPClient) Steer(ctx context.Context, text string) (mechanism string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	sid := c.sessionID
	if c.closed {
		c.mu.Unlock()
		return "", fmt.Errorf("cursor acp: client closed")
	}
	if sid == "" {
		c.mu.Unlock()
		return "", fmt.Errorf("cursor acp: no session")
	}
	mechanism = acpSubmitMechanism
	if c.prompts.inFlight() {
		mechanism = acpSteerMechanism
	}
	id := atomic.AddInt64(&c.nextID, 1)
	c.prompts.push(id)
	c.mu.Unlock()
	publishEvent(c.onEvent, acpPromptAcceptedEvent(sid, id))
	if err := c.write(acpPromptRequest(id, sid, text)); err != nil {
		return "", err
	}
	return mechanism, nil
}

// SupersededTurnID is the turn id the most recent Steer pushed over, or
// "" when no steer has happened on this client.
func (c *cursorACPClient) SupersededTurnID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prompts.supersededTurnID()
}

func (c *cursorACPClient) publishPromptResult(msg acpRPCMessage, sessionID, turnID string) {
	if msg.Error != nil {
		if c.onEvent != nil {
			raw, _ := json.Marshal(msg.Error)
			c.onEvent(Event{
				Type:       "assistant",
				SessionID:  sessionID,
				TurnID:     turnID,
				Raw:        raw,
				Text:       msg.Error.Message,
				StopReason: "end_turn",
			})
		}
		return
	}
	stopReason := "end_turn"
	var usage Usage
	var meta struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
		} `json:"usage"`
		Meta *struct {
			InputTokens      int `json:"inputTokens"`
			OutputTokens     int `json:"outputTokens"`
			CachedReadTokens int `json:"cachedReadTokens"`
		} `json:"_meta"`
	}
	if len(msg.Result) > 0 {
		if err := json.Unmarshal(msg.Result, &meta); err == nil {
			if meta.StopReason != "" {
				stopReason = meta.StopReason
			}
			if meta.Meta != nil {
				usage = Usage{
					InputTokens:          meta.Meta.InputTokens,
					OutputTokens:         meta.Meta.OutputTokens,
					CacheReadInputTokens: meta.Meta.CachedReadTokens,
				}
			} else if meta.Usage != nil {
				usage = Usage{
					InputTokens:  meta.Usage.InputTokens,
					OutputTokens: meta.Usage.OutputTokens,
				}
			}
		}
	}
	switch stopReason {
	case "end_turn", "stop_sequence", "max_tokens":
	case "cancelled", "refusal":
		stopReason = "end_turn"
	default:
		stopReason = "end_turn"
	}
	if c.onEvent != nil {
		raw := msg.Result
		if len(raw) == 0 {
			raw, _ = json.Marshal(map[string]any{"stopReason": stopReason})
		}
		c.onEvent(Event{
			Type:       "assistant",
			SessionID:  sessionID,
			TurnID:     turnID,
			Raw:        raw,
			StopReason: stopReason,
			Usage:      usage,
		})
	}
}

func (c *cursorACPClient) Cancel() error {
	c.mu.Lock()
	sid := c.sessionID
	c.prompts.clear()
	c.mu.Unlock()
	if sid == "" {
		return nil
	}
	return c.notify("session/cancel", map[string]any{"sessionId": sid})
}

// SetModel switches the ACP session model (🎯T54).
func (c *cursorACPClient) SetModel(model string) error {
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	return acpSetModel(c.request, sid, model)
}

func (c *cursorACPClient) promptInFlight() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prompts.inFlight()
}

func (c *cursorACPClient) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
		c.wakePromptWaiters()
		// These handles are immutable after construction. Never acquire writeMu:
		// the write we need to interrupt may hold it indefinitely.
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.stdout != nil {
			_ = c.stdout.Close()
		}
		if c.stderr != nil {
			_ = c.stderr.Close()
		}
		// EOF alone does not release Cursor's store.db writer.
		if c.ownsProcess && c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			_, _ = c.cmd.Process.Wait()
		}
	})
}

func (c *cursorACPClient) nextReqID() int64 {
	return atomic.AddInt64(&c.nextID, 1)
}

func (c *cursorACPClient) request(method string, params any) (json.RawMessage, error) {
	return c.requestContext(context.Background(), method, params)
}

func (c *cursorACPClient) requestContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	closed := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() { c.Close(); close(closed) })
	defer func() {
		if !stopCancel() {
			<-closed
		}
	}()
	id := c.nextReqID()
	ch := make(chan acpRPCMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("cursor acp: client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if pj, err := json.Marshal(params); err == nil {
		slog.Debug("cursor acp request", "method", method, "params", string(pj))
	}
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if err := c.write(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	resp, ok := <-ch
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !ok {
		return nil, fmt.Errorf("cursor acp: connection closed waiting for %s", method)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("acp %s: %s", method, resp.Error.Message)
	}
	return resp.Result, nil
}

func (c *cursorACPClient) notify(method string, params any) error {
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (c *cursorACPClient) reply(id *int64, result any) error {
	if id == nil {
		return nil
	}
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      *id,
		"result":  result,
	})
}

func (c *cursorACPClient) replyError(id *int64, code int, message string) error {
	if id == nil {
		return nil
	}
	return c.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      *id,
		"error":   map[string]any{"code": code, "message": message},
	})
}

func (c *cursorACPClient) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return fmt.Errorf("cursor acp: client closed")
	}
	if c.stdin == nil {
		return fmt.Errorf("cursor acp: no transport")
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

// cursorSessionPlan is everything the Cursor Session path derives from a
// start request before it launches anything. Splitting it out lets the
// request-field audit materialise the request without an agent binary.
type cursorSessionPlan struct {
	Args            []string
	WorkDir         string
	Model           string
	PreferSessionID string
	RequireResume   bool
	MCPServers      []any
	// MCPExclusive is the isolate flag from Config. Cursor has no
	// strict-mcp flag and Claudia does not rewrite project mcp.json;
	// exclusive Session MCP is ACP mcpServers only.
	MCPExclusive bool
}

func planCursorSession(req agentStartRequest) cursorSessionPlan {
	preferID := ""
	if req.Resuming || req.Config.SessionID != "" {
		preferID = req.SessionID
	}
	return cursorSessionPlan{
		Args:            cursorACPArgs(req.Config.Model),
		WorkDir:         req.WorkDir,
		Model:           req.Config.Model,
		PreferSessionID: preferID,
		RequireResume:   req.Config.RequireResume,
		MCPServers:      resolveACPMCPServers(req.Config),
		MCPExclusive:    req.Config.MCPExclusive,
	}
}

func cursorSessionPrecheck(req agentStartRequest) error {
	if len(req.Config.DisallowTools) > 0 {
		return capabilityRefusal(ProviderCursor, CapabilityToolRestrictions, cursorToolRestrictionsReason)
	}
	if mode := req.Config.PermissionMode; mode != "" && mode != "bypassPermissions" {
		return capabilityRefusal(ProviderCursor, CapabilityPermissionMode, cursorPermissionModeReason)
	}
	if len(req.Config.ExtraArgs) > 0 {
		return capabilityRefusal(ProviderCursor, CapabilityExtraArgs, cursorExtraArgsReason)
	}
	if sandboxPolicyRequested(req.Config) {
		return capabilityRefusal(ProviderCursor, CapabilitySandboxPolicy, sandboxPolicyIsCodexOnlyReason)
	}
	return nil
}

func startCursorAgent(req agentStartRequest) (*agentStart, error) {
	if err := cursorSessionPrecheck(req); err != nil {
		return nil, err
	}

	bin, err := resolveCursorBin()
	if err != nil {
		return nil, err
	}

	plan := planCursorSession(req)
	// Stdio cannot be adopted after the coordinator dies. Reap leftover
	// writers (persisted PID + anyone holding store.db) before minting
	// so Launch cannot stack a second client (🎯T541.1).
	if req.Config.ConnectPID > 0 && req.Config.ConnectURL == "" {
		ReapCursorACPLeftovers(plan.PreferSessionID, req.Config.ConnectPID)
	} else if plan.PreferSessionID != "" {
		ReapCursorACPLeftovers(plan.PreferSessionID, 0)
	}
	var bind acpBind

	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	client, err := startCursorACP(ctx, bin, plan.WorkDir, plan.Model, plan.PreferSessionID, plan.RequireResume, plan.MCPServers, nil, bind.onEvent, bind.onClose)
	if err != nil {
		if plan.PreferSessionID != "" {
			ReapCursorACPLeftovers(plan.PreferSessionID, 0)
		}
		return nil, err
	}

	sid := client.SessionID()
	pid := 0
	if client.cmd != nil && client.cmd.Process != nil {
		pid = client.cmd.Process.Pid
	}
	ops := agentOps{
		attachCommand: func(*Agent) string { return "" },
		interrupt: func(*Agent) error {
			return client.Cancel()
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
		// 🎯T72.2 seam over the 🎯T72.1 prompt-id stack.
		steer: steerOp(client),
	}
	return &agentStart{
		WindowID:   "cursor-acp-" + sid,
		Ops:        ops,
		TailJSONL:  false,
		SessionID:  sid,
		ConnectPID: pid,
		DetectReady: func(a *Agent) {
			bind.attach(a)
			select {
			case <-a.ready:
			default:
				close(a.ready)
			}
		},
	}, nil
}
