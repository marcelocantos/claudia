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
	"syscall"
	"time"
)

// mcpStdioBackend is one long-lived stdio MCP process exposed as
// streamable-HTTP JSON-RPC (🎯T2.16). Seats POST; this process writes
// newline-delimited (or Content-Length) frames to stdin and returns the
// matching id.
type mcpStdioBackend struct {
	srv MCPServer

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func newMCPStdioBackend(s MCPServer) *mcpStdioBackend {
	return &mcpStdioBackend{srv: s}
}

func (b *mcpStdioBackend) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.killLocked()
}

func (b *mcpStdioBackend) killLocked() {
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		_, _ = b.cmd.Process.Wait()
	}
	if b.stdin != nil {
		_ = b.stdin.Close()
	}
	b.cmd = nil
	b.stdin = nil
	b.stdout = nil
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
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
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
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("mcp stdio: request: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureLocked(); err != nil {
		return nil, err
	}
	if _, err := b.stdin.Write(append(raw, '\n')); err != nil {
		b.killLocked()
		if err := b.ensureLocked(); err != nil {
			return nil, err
		}
		if _, err := b.stdin.Write(append(raw, '\n')); err != nil {
			return nil, fmt.Errorf("mcp stdio: write: %w", err)
		}
	}
	if len(bytes.TrimSpace(req.ID)) == 0 || string(req.ID) == "null" {
		return []byte(`{"jsonrpc":"2.0"}`), nil
	}
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	for time.Now().Before(deadline) {
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		_ = setReadDeadline(b.cmd, remain)
		frame, err := readMCPFrame(b.stdout)
		if err != nil {
			b.killLocked()
			return nil, fmt.Errorf("mcp stdio: read: %w", err)
		}
		var got struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(frame, &got); err != nil {
			continue
		}
		if len(bytes.TrimSpace(got.ID)) == 0 {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(got.ID), bytes.TrimSpace(req.ID)) {
			return frame, nil
		}
	}
	return nil, fmt.Errorf("mcp stdio: no reply for id %s", string(req.ID))
}

func (b *mcpStdioBackend) ensureLocked() error {
	if b.cmd != nil && b.cmd.Process != nil {
		if err := b.cmd.Process.Signal(syscall.Signal(0)); err == nil {
			return nil
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
		return fmt.Errorf("mcp stdio %s: stdin: %w", b.srv.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("mcp stdio %s: stdout: %w", b.srv.Name, err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("mcp stdio %s: start: %w", b.srv.Name, err)
	}
	b.cmd = cmd
	b.stdin = stdin
	b.stdout = bufio.NewReader(stdout)
	return nil
}

func setReadDeadline(cmd *exec.Cmd, _ time.Duration) error {
	// Process pipes do not expose a deadline on every OS; the context
	// timeout in ServeHTTP is the bound. Kept as a hook for tests.
	_ = cmd
	return nil
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
