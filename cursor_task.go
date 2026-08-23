// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

type cursorTaskBackend struct{}

func (cursorTaskBackend) Capabilities() providerCapabilities {
	return providerCapabilities{
		Task:   true,
		Resume: true,
	}
}

// cursorTaskArgs builds argv for `agent --print --output-format stream-json`.
// Root flags precede the prompt. Resume uses --resume <chatId> when
// TaskConfig.ClaudeID / req.SessionID is set.
func cursorTaskArgs(req taskRunRequest) []string {
	args := []string{"--print", "--output-format", "stream-json", "--force", "--trust"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	return append(args, req.Prompt)
}

func cursorTaskPrecheck(req taskRunRequest) error {
	if len(req.DisallowTools) > 0 {
		return capabilityRefusal(ProviderCursor, CapabilityToolRestrictions, cursorToolRestrictionsReason)
	}
	if req.SandboxMode != "" {
		return capabilityRefusal(ProviderCursor, CapabilitySandboxPolicy, sandboxPolicyIsCodexOnlyReason)
	}
	if req.ApprovalPolicy != "" {
		return capabilityRefusal(ProviderCursor, CapabilityPermissionMode,
			"Cursor Task has no ApprovalPolicy flag; use agent defaults or Session ACP auto-approve")
	}
	return nil
}

func (cursorTaskBackend) RunTask(ctx context.Context, req taskRunRequest) (*taskRun, error) {
	if err := cursorTaskPrecheck(req); err != nil {
		return nil, err
	}

	bin, err := resolveCursorBin()
	if err != nil {
		return nil, err
	}
	args := cursorTaskArgs(req)
	slog.Debug("spawning cursor task", "bin", bin, "args", args)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = req.WorkDir
	setTaskProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cursor task stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("cursor task stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cursor task: %w", err)
	}
	armTaskProcessGroupKill(ctx, cmd)

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 256*1024), 256*1024)
		for sc.Scan() {
			slog.Debug("cursor task stderr", "line", sc.Text())
		}
	}()

	ch := make(chan TaskEvent, 16)
	go func() {
		defer close(ch)
		defer func() {
			if err := cmd.Wait(); err != nil {
				slog.Warn("cursor task process exited with error", "error", err)
			}
		}()
		forwardTaskStream(ctx, stdout, ParseCursorTaskLine, req.RawLog, ch)
	}()

	return &taskRun{
		events:    ch,
		interrupt: func() error { return interruptTaskProcess(cmd) },
	}, nil
}

// ParseCursorTaskLine parses one NDJSON line from
// `agent --print --output-format stream-json`. The envelope matches Claude's
// stream-json closely, but usage fields are camelCase (inputTokens, …).
func ParseCursorTaskLine(line []byte) []TaskEvent {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &base); err != nil {
		return nil
	}
	switch base.Type {
	case "system":
		return parseTaskSystem(line)
	case "assistant":
		return parseTaskAssistant(line)
	case "result":
		return parseCursorTaskResult(line)
	default:
		// thinking / user / tool progress: ignore for Task event surface
		return nil
	}
}

func parseCursorTaskResult(line []byte) []TaskEvent {
	var msg struct {
		Subtype    string  `json:"subtype"`
		Result     string  `json:"result"`
		IsError    bool    `json:"is_error"`
		DurationMs float64 `json:"duration_ms"`
		Usage      *struct {
			InputTokens      int `json:"inputTokens"`
			OutputTokens     int `json:"outputTokens"`
			CacheReadTokens  int `json:"cacheReadTokens"`
			CacheWriteTokens int `json:"cacheWriteTokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil
	}
	if msg.IsError || msg.Subtype != "success" {
		errMsg := strings.TrimSpace(msg.Result)
		if errMsg == "" {
			errMsg = "cursor task failed"
		}
		return []TaskEvent{{
			Type:     TaskEventError,
			IsError:  true,
			ErrorMsg: errMsg,
		}}
	}
	var usage Usage
	if msg.Usage != nil {
		usage = Usage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadTokens,
			CacheCreationInputTokens: msg.Usage.CacheWriteTokens,
		}
	}
	return []TaskEvent{{
		Type:       TaskEventResult,
		Content:    msg.Result,
		DurationMs: msg.DurationMs,
		Usage:      usage,
	}}
}
