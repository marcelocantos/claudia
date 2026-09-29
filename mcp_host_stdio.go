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
	"sync/atomic"
	"time"
)

// mcpStdioBackend is one long-lived stdio MCP process exposed as
// streamable-HTTP JSON-RPC (🎯T2.16). Seats POST; this process writes
// newline-delimited frames to stdin, and one reader routes each reply back
// to the request waiting on it.
//
// Every seat that names the recipe shares the process, and seats number
// their requests from the same small integers, so each request goes to the
// process under an id of the backend's own and gets its caller's id back on
// the reply. The lock covers the stdin write only: a request the process
// never answers fails at its own deadline and holds up nothing else
// (jevons 🎯T928 — one unanswered pre-initialize server/discover used to
// hold the lock on a read with no deadline and leave the server dead for
// every seat until the broker restarted).
type mcpStdioBackend struct {
	srv  MCPServer
	next atomic.Int64

	mu   sync.Mutex // guards proc; serialises stdin writes
	proc *mcpStdioProc
}

// mcpStdioProc is one run of the backend's process.
type mcpStdioProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{} // closed when the reader stops: the process is gone
	stop  sync.Once

	mu          sync.Mutex
	waiters     map[string]chan []byte // backend id -> the request waiting on it
	initialized bool
}

// mcpStdioRequestTimeout bounds a request whose caller set no deadline.
var mcpStdioRequestTimeout = 20 * time.Second

// mcpStdioTimeoutCode is the JSON-RPC error a request gets when the process
// has not answered it by its deadline.
const mcpStdioTimeoutCode = -32001

func newMCPStdioBackend(s MCPServer) *mcpStdioBackend {
	return &mcpStdioBackend{srv: s}
}

func (b *mcpStdioBackend) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.killLocked()
}

func (b *mcpStdioBackend) killLocked() {
	if b.proc != nil {
		b.proc.kill()
	}
	b.proc = nil
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
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, fmt.Sprintf("mcp stdio: request: %v", err), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mcpStdioRequestTimeout)
		defer cancel()
	}
	resp, err := b.roundTrip(ctx, body, msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}

func (b *mcpStdioBackend) roundTrip(ctx context.Context, raw []byte, msg map[string]json.RawMessage) ([]byte, error) {
	id := bytes.TrimSpace(msg["id"])
	var method string
	if m, ok := msg["method"]; ok {
		if err := json.Unmarshal(m, &method); err != nil {
			return nil, fmt.Errorf("mcp stdio: request method: %w", err)
		}
	}
	// A notification, or a seat's answer to a server-to-client request:
	// nothing comes back for either.
	if len(id) == 0 || string(id) == "null" || method == "" {
		if _, err := b.send(raw, "", nil); err != nil {
			return nil, err
		}
		return []byte(`{"jsonrpc":"2.0"}`), nil
	}
	// Claude Code opens with server/discover before initialize. A stdio
	// server drops anything that arrives before initialize, so the seat
	// would wait out its probe timeout for nothing; answer for the process
	// until it has been initialized.
	if method == "server/discover" && !b.initialized() {
		return mcpJSONRPCError(id, -32601, "method not found: server/discover"), nil
	}

	backendID := strconv.FormatInt(b.next.Add(1), 10)
	msg["id"] = json.RawMessage(backendID)
	line, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("mcp stdio: request: %w", err)
	}
	reply := make(chan []byte, 1)
	proc, err := b.send(line, backendID, reply)
	if err != nil {
		return nil, err
	}
	var frame []byte
	select {
	case frame = <-reply:
	case <-proc.done:
		select {
		case frame = <-reply:
		default:
			return nil, fmt.Errorf("mcp stdio %s: process exited before answering %s", b.srv.Name, method)
		}
	case <-ctx.Done():
		proc.forget(backendID)
		return mcpJSONRPCError(id, mcpStdioTimeoutCode,
			fmt.Sprintf("mcp stdio %s: no reply to %s before the deadline", b.srv.Name, method)), nil
	}
	if method == "initialize" && mcpFrameIsResult(frame) {
		proc.markInitialized()
	}
	return mcpWithID(frame, id)
}

// send writes one frame, registering reply under backendID first so a fast
// answer cannot beat the registration. A write that fails restarts the
// process and tries once more.
func (b *mcpStdioBackend) send(line []byte, backendID string, reply chan []byte) (*mcpStdioProc, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	line = append(line, '\n')
	for attempt := 0; ; attempt++ {
		proc, err := b.ensureLocked()
		if err != nil {
			return nil, err
		}
		if reply != nil {
			proc.register(backendID, reply)
		}
		_, err = proc.stdin.Write(line)
		if err == nil {
			return proc, nil
		}
		proc.forget(backendID)
		b.killLocked()
		if attempt > 0 {
			return nil, fmt.Errorf("mcp stdio %s: write: %w", b.srv.Name, err)
		}
	}
}

func (b *mcpStdioBackend) initialized() bool {
	b.mu.Lock()
	proc := b.proc
	b.mu.Unlock()
	if proc == nil {
		return false
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	return proc.initialized
}

func (b *mcpStdioBackend) ensureLocked() (*mcpStdioProc, error) {
	if b.proc != nil {
		select {
		case <-b.proc.done:
		default:
			return b.proc, nil
		}
	}
	b.killLocked()
	cmd := exec.Command(b.srv.Command, b.srv.Args...)
	env := os.Environ()
	for k, v := range b.srv.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp stdio %s: stdin: %w", b.srv.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp stdio %s: stdout: %w", b.srv.Name, err)
	}
	// Stderr stays nil (the null device): a copying goroutine would make
	// Wait hang on any grandchild that inherited the pipe.
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp stdio %s: start: %w", b.srv.Name, err)
	}
	proc := &mcpStdioProc{
		cmd:     cmd,
		stdin:   stdin,
		done:    make(chan struct{}),
		waiters: map[string]chan []byte{},
	}
	go proc.read(bufio.NewReader(stdout))
	b.proc = proc
	return proc, nil
}

// read routes every reply the process writes to the request waiting on its
// id. Frames carrying a method are the server's own requests and
// notifications; no seat is listening for them here, so they are dropped.
func (p *mcpStdioProc) read(r *bufio.Reader) {
	defer p.kill()
	defer close(p.done)
	for {
		frame, err := readMCPFrame(r)
		if err != nil {
			return
		}
		var got struct {
			ID     json.RawMessage `json:"id"`
			Method json.RawMessage `json:"method"`
		}
		if json.Unmarshal(frame, &got) != nil || len(got.Method) != 0 {
			continue
		}
		key := string(bytes.TrimSpace(got.ID))
		p.mu.Lock()
		reply := p.waiters[key]
		delete(p.waiters, key)
		p.mu.Unlock()
		if reply != nil {
			reply <- frame
		}
	}
}

// kill ends the process and reaps it. Wait also closes stdout, which ends
// the reader.
func (p *mcpStdioProc) kill() {
	p.stop.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.stdin.Close()
		_ = p.cmd.Wait()
	})
}

func (p *mcpStdioProc) register(backendID string, reply chan []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waiters[backendID] = reply
}

func (p *mcpStdioProc) forget(backendID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.waiters, backendID)
}

func (p *mcpStdioProc) markInitialized() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initialized = true
}

func mcpFrameIsResult(frame []byte) bool {
	var got struct {
		Result json.RawMessage `json:"result"`
	}
	return json.Unmarshal(frame, &got) == nil && len(got.Result) != 0
}

// mcpWithID puts the caller's own id back on a reply.
func mcpWithID(frame, id []byte) ([]byte, error) {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(frame, &msg); err != nil {
		return nil, fmt.Errorf("mcp stdio: reply: %w", err)
	}
	msg["id"] = json.RawMessage(id)
	return json.Marshal(msg)
}

func mcpJSONRPCError(id []byte, code int, message string) []byte {
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
	return out
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
