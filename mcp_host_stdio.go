// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mcpStdioBackend is one long-lived stdio MCP process exposed as
// streamable-HTTP JSON-RPC (🎯T2.16). Seats POST; this process writes
// newline-delimited frames to stdin and returns the reply whose id matches.
//
// Every seat shares the one process, so the backend owns the session
// (jevons 🎯T928):
//
//   - It performs the initialize handshake itself when it starts the
//     process, before any seat's request is written. A server that queues
//     requests until it is initialized (mcpbridge does) otherwise holds a
//     bare tools/list forever when that is the first request it sees.
//   - Requests are in flight concurrently. Each gets a host-assigned id on
//     the wire, because every seat numbers its own requests from zero, and
//     one reader routes each reply back to the request that sent it.
//   - No lock is held across process I/O. A request that gets no reply
//     fails at its own deadline and cannot stop any other request.
//
// Before 🎯T928 a request held the backend's mutex while it blocked reading
// the reply. omp's bare tools/list reached a fresh mcpbridge first, mcpbridge
// queued it waiting for initialize, and the initialize that would have
// released it could never be written: bullseye, sawmill, spyder and vellum
// hung for every seat on the host until the broker restarted.
type mcpStdioBackend struct {
	srv MCPServer

	mu   sync.Mutex
	proc *mcpStdioProc
}

// mcpStdioProc is one running process and the requests waiting on it.
type mcpStdioProc struct {
	name   string
	cmd    *exec.Cmd
	writes chan []byte
	// done is closed when the reader stops: the process exited, closed its
	// stdout, or was killed. Nothing more will be answered.
	done chan struct{}
	// ready is closed when the host's own initialize has been answered, or
	// has failed; requests from seats are written only after it.
	ready chan struct{}

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan []byte
}

// mcpStdioRequestTimeout bounds a request whose HTTP context carries no
// deadline, which is every request from a seat.
const mcpStdioRequestTimeout = 20 * time.Second

// mcpStdioInitTimeout bounds the host's own initialize. A server that does
// not answer it still gets the seats' requests afterwards; it may answer
// those.
const mcpStdioInitTimeout = 30 * time.Second

// mcpStdioWriteQueue is how many frames may wait for the writer before a
// request is refused rather than queued.
const mcpStdioWriteQueue = 64

// mcpHostProtocolVersion is what the host asks for in its own initialize.
const mcpHostProtocolVersion = "2025-06-18"

// JSON-RPC error codes the host sends in reply to requests from the server.
const jsonRPCMethodNotFound = -32601

func newMCPStdioBackend(s MCPServer) *mcpStdioBackend {
	return &mcpStdioBackend{srv: s}
}

func (b *mcpStdioBackend) close() {
	b.mu.Lock()
	p := b.proc
	b.proc = nil
	b.mu.Unlock()
	if p != nil {
		p.kill()
	}
}

func (b *mcpStdioBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mcpStdioRequestTimeout)
		defer cancel()
	}
	resp, err := b.roundTrip(ctx, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func (b *mcpStdioBackend) roundTrip(ctx context.Context, raw []byte) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, fmt.Errorf("mcp stdio: request: %w", err)
	}
	p, err := b.running()
	if err != nil {
		return nil, err
	}
	select {
	case <-p.ready:
	case <-p.done:
		return nil, fmt.Errorf("mcp stdio %s: process exited", b.srv.Name)
	case <-ctx.Done():
		return nil, fmt.Errorf("mcp stdio %s: not initialized: %w", b.srv.Name, ctx.Err())
	}
	origID := bytes.TrimSpace(msg["id"])
	if len(origID) == 0 || string(origID) == "null" {
		if err := p.write(ctx, raw); err != nil {
			return nil, err
		}
		return []byte(`{"jsonrpc":"2.0"}`), nil
	}
	reply, err := p.call(ctx, msg)
	if err != nil {
		return nil, fmt.Errorf("mcp stdio %s: id %s: %w", b.srv.Name, origID, err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(reply, &out); err != nil {
		return nil, fmt.Errorf("mcp stdio %s: reply: %w", b.srv.Name, err)
	}
	out["id"] = origID
	return json.Marshal(out)
}

// running returns the live process, starting one when there is none or the
// last one has stopped.
func (b *mcpStdioBackend) running() (*mcpStdioProc, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proc != nil {
		select {
		case <-b.proc.done:
			b.proc.kill()
			b.proc = nil
		default:
			return b.proc, nil
		}
	}
	p, err := startMCPStdioProc(b.srv)
	if err != nil {
		return nil, err
	}
	b.proc = p
	return p, nil
}

func startMCPStdioProc(s MCPServer) (*mcpStdioProc, error) {
	cmd := exec.Command(s.Command, s.Args...)
	env := os.Environ()
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio %s: stdin: %w", s.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp stdio %s: stdout: %w", s.Name, err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp stdio %s: start: %w", s.Name, err)
	}
	p := &mcpStdioProc{
		name:    s.Name,
		cmd:     cmd,
		writes:  make(chan []byte, mcpStdioWriteQueue),
		done:    make(chan struct{}),
		ready:   make(chan struct{}),
		pending: map[int64]chan []byte{},
	}
	go p.writeLoop(stdin)
	go p.readLoop(bufio.NewReader(stdout))
	go p.initialize()
	return p, nil
}

// initialize is the host's own handshake, so the server is initialized
// before any seat's request reaches it, whatever that request is.
func (p *mcpStdioProc) initialize() {
	defer close(p.ready)
	ctx, cancel := context.WithTimeout(context.Background(), mcpStdioInitTimeout)
	defer cancel()
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": mcpHostProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "claudia-mcp-host", "version": "1"},
	})
	if _, err := p.call(ctx, map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"method":  json.RawMessage(`"initialize"`),
		"params":  params,
	}); err != nil {
		return
	}
	_ = p.write(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
}

// call writes msg under a fresh host id and waits for the reply to it.
func (p *mcpStdioProc) call(ctx context.Context, msg map[string]json.RawMessage) ([]byte, error) {
	ch := make(chan []byte, 1)
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()
	wire := make(map[string]json.RawMessage, len(msg))
	for k, v := range msg {
		wire[k] = v
	}
	wire["id"] = json.RawMessage(strconv.FormatInt(id, 10))
	frame, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	if err := p.write(ctx, frame); err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply, nil
	case <-p.done:
		return nil, fmt.Errorf("process exited before replying")
	case <-ctx.Done():
		return nil, fmt.Errorf("no reply: %w", ctx.Err())
	}
}

// write hands one frame to the writer.
func (p *mcpStdioProc) write(ctx context.Context, frame []byte) error {
	select {
	case p.writes <- frame:
		return nil
	case <-p.done:
		return fmt.Errorf("mcp stdio %s: process exited", p.name)
	case <-ctx.Done():
		return fmt.Errorf("mcp stdio %s: write: %w", p.name, ctx.Err())
	}
}

// writeLoop is the only writer to the process's stdin, so frames never
// interleave. A write error means the process is going away; the reader
// sees that and closes done.
func (p *mcpStdioProc) writeLoop(stdin io.WriteCloser) {
	defer stdin.Close()
	for {
		select {
		case frame := <-p.writes:
			if _, err := stdin.Write(append(frame, '\n')); err != nil {
				p.kill()
				return
			}
		case <-p.done:
			return
		}
	}
}

// readLoop routes each reply to the request that is waiting for it and
// answers the server's own requests. It owns cmd.Wait.
func (p *mcpStdioProc) readLoop(r *bufio.Reader) {
	defer func() {
		close(p.done)
		_ = p.cmd.Wait()
	}()
	for {
		frame, err := readMCPFrame(r)
		if err != nil {
			return
		}
		var got struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(frame, &got) != nil {
			continue
		}
		id := bytes.TrimSpace(got.ID)
		if got.Method != "" {
			if len(id) > 0 && string(id) != "null" {
				p.answerServerRequest(id, got.Method)
			}
			continue // a notification: no seat is listening for these
		}
		n, err := strconv.ParseInt(string(id), 10, 64)
		if err != nil {
			continue // not an id this host sent
		}
		p.mu.Lock()
		ch := p.pending[n]
		delete(p.pending, n)
		p.mu.Unlock()
		if ch != nil {
			ch <- frame
		}
	}
}

// answerServerRequest replies to a request the server sent the host, so a
// server that waits on it is not left waiting. The host has no roots,
// sampling or elicitation to offer; it does answer ping.
func (p *mcpStdioProc) answerServerRequest(id json.RawMessage, method string) {
	var reply []byte
	if method == "ping" {
		reply, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
	} else {
		reply, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": jsonRPCMethodNotFound, "message": "not supported by claudia mcp host: " + method}})
	}
	select {
	case p.writes <- reply:
	default: // writer backed up; the server will time out its own request
	}
}

func (p *mcpStdioProc) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func readMCPFrame(r *bufio.Reader) ([]byte, error) {
	prefix, err := r.Peek(16)
	if err != nil && err != io.EOF && len(prefix) == 0 {
		return nil, err
	}
	if bytes.HasPrefix(bytes.ToLower(prefix), []byte("content-length:")) {
		return readMCPContentLength(r)
	}
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	return bytes.TrimSpace(line), nil
}

func readMCPContentLength(r *bufio.Reader) ([]byte, error) {
	var n int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		low := strings.ToLower(line)
		if strings.HasPrefix(low, "content-length:") {
			v := strings.TrimSpace(line[len("Content-Length:"):])
			n, err = strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("mcp stdio: content-length: %w", err)
			}
		}
	}
	if n <= 0 {
		return nil, fmt.Errorf("mcp stdio: missing content-length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
