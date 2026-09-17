// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/marcelocantos/claudia/internal/broker"
)

// The broker-backed Task backend (🎯T2.10, Task first). One run is one
// connection: task_run, then a stream of task_event ending in task_done. The
// daemon spawns the provider process, so a consumer that dies mid-run leaves
// the daemon to cancel it (connection gone) rather than an orphan.

// brokerTaskBackend runs one turn through the daemon.
type brokerTaskBackend struct {
	cfg    TaskConfig
	client *brokerClient
}

// Capabilities is the provider's own matrix.
func (b *brokerTaskBackend) Capabilities() providerCapabilities {
	return taskBackendForProvider(b.cfg.Provider).Capabilities()
}

// RunTask sends task_run and adapts the stream to a taskRun. A raw-log func
// asks the daemon for the provider's raw lines, which arrive as task_raw
// pushes and are delivered to it in order (🎯T75.10).
func (b *brokerTaskBackend) RunTask(ctx context.Context, req taskRunRequest) (*taskRun, error) {
	cfg := b.cfg
	cfg.WorkDir = req.WorkDir
	cfg.Model = req.Model
	cfg.SandboxMode = req.SandboxMode
	cfg.ApprovalPolicy = req.ApprovalPolicy
	cfg.DisallowTools = req.DisallowTools
	cfg.ClaudeID = req.SessionID
	raw, err := encodeTaskConfigWire(cfg)
	if err != nil {
		return nil, err
	}
	events := make(chan TaskEvent, 16)
	done := make(chan struct{})
	var runID string
	b.client.setPush(func(resp *broker.Response) {
		switch resp.Type {
		case broker.TypeTaskEvent:
			ev, err := decodeTaskEventWire(resp.TaskEvent.Event)
			if err != nil {
				slog.Warn("broker task event undecodable", "run", runID, "err", err)
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		case broker.TypeTaskRaw:
			if req.RawLog != nil {
				req.RawLog([]byte(resp.TaskRaw.Line))
			}
		case broker.TypeTaskDone:
			if resp.TaskDone.Error != "" {
				select {
				case events <- TaskEvent{Type: TaskEventError, IsError: true, ErrorMsg: resp.TaskDone.Error}:
				case <-ctx.Done():
				}
			}
			close(done)
		}
	})
	resp, err := b.client.call(ctx, &broker.Request{Type: broker.TypeTaskRun, TaskRun: &broker.TaskRunRequest{Task: raw, Prompt: req.Prompt, RawLog: req.RawLog != nil}})
	if err != nil {
		return nil, err
	}
	if resp.TaskStarted == nil {
		return nil, fmt.Errorf("broker: task_run answered with %s", resp.Type)
	}
	runID = resp.TaskStarted.RunID
	go func() {
		defer close(events)
		defer b.client.Close()
		select {
		case <-done:
		case <-ctx.Done():
		case <-b.client.done:
			// Connection dropped under the run: the consumer must not
			// mistake a truncated stream for a finished one.
			select {
			case events <- TaskEvent{Type: TaskEventError, IsError: true, ErrorMsg: "broker connection lost during run"}:
			default:
			}
		}
	}()
	return &taskRun{
		events: events,
		interrupt: func() error {
			_, err := b.client.callTimeout(&broker.Request{Type: broker.TypeTaskCancel, TaskCancel: &broker.TaskCancelRequest{RunID: runID}}, brokerOpTimeout)
			return err
		},
	}, nil
}

// newRunID is a short random id for anonymous grants and daemon run ids.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "r0"
	}
	return hex.EncodeToString(b[:])
}
