// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	cfg.SandboxGitWrite = req.SandboxGitWrite
	cfg.ApprovalPolicy = req.ApprovalPolicy
	cfg.DisallowTools = req.DisallowTools
	cfg.ClaudeID = req.SessionID
	raw, err := EncodeTaskConfigWire(cfg)
	if err != nil {
		return nil, err
	}
	events := make(chan TaskEvent, 16)
	done := make(chan struct{})
	var runID string
	b.client.setPush(func(resp *broker.Response) {
		switch resp.Type {
		case broker.TypeTaskEvent:
			ev, err := DecodeTaskEventWire(resp.TaskEvent.Event)
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
	pick := ""
	if cfg.PickByRemaining {
		pick = PickRemaining
	}
	resp, err := b.client.call(ctx, &broker.Request{Type: broker.TypeTaskRun, TaskRun: &broker.TaskRunRequest{Task: raw, Prompt: req.Prompt, RawLog: req.RawLog != nil, Pick: pick}})
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

// ErrPlanExhausted means the broker refused task_run before spawning:
// its plan-usage snapshot shows that provider has no usable capacity.
// `claudia broker usage` prints the snapshot the refusal was decided from.
var ErrPlanExhausted = errors.New("claudia: plan exhausted")

// RunBrokerTask runs one Task turn on the host broker. It dials the broker
// socket, sends task_run, and returns the task_event stream, which ends
// after task_done. It does not start a provider in this process: no daemon
// is [ErrNoBroker], and a bare protocol server is not a fallback either.
//
// The daemon admits the run against its plan-usage snapshot before spawning.
// That snapshot is what `claudia broker usage` prints. A provider the snapshot
// shows as exhausted comes back as [ErrPlanExhausted] and nothing is spawned.
// A provider with no row is admitted.
//
// Cancel ctx to drop the connection; the daemon then cancels the run. Read
// the channel to completion or cancel — the same rule as [Task.Run].
func RunBrokerTask(ctx context.Context, prompt string, cfg TaskConfig) (<-chan TaskEvent, error) {
	client, err := dialBroker()
	if err != nil {
		return nil, err
	}
	bb := &brokerTaskBackend{cfg: cfg, client: client}
	run, err := bb.RunTask(ctx, taskRunRequest{
		WorkDir:         cfg.WorkDir,
		Model:           cfg.Model,
		SandboxMode:     cfg.SandboxMode,
		SandboxGitWrite: cfg.SandboxGitWrite,
		ApprovalPolicy:  cfg.ApprovalPolicy,
		DisallowTools:   cfg.DisallowTools,
		SessionID:       cfg.ClaudeID,
		Prompt:          prompt,
	})
	if err != nil {
		client.Close()
		return nil, brokerTaskError(err)
	}
	ch := make(chan TaskEvent, 16)
	go func() {
		defer close(ch)
		for ev := range run.events {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// brokerTaskError maps a task_run refusal onto the public sentinels.
func brokerTaskError(err error) error {
	var pe *broker.ProtocolError
	if errors.As(err, &pe) && pe.Code == broker.CodePlanExhausted {
		return fmt.Errorf("%w: %s", ErrPlanExhausted, pe.Error())
	}
	return err
}

// newRunID is a short random id for anonymous grants and daemon run ids.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "r0"
	}
	return hex.EncodeToString(b[:])
}
