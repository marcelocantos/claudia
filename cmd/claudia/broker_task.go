// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

// taskCmd is one task_run on the broker socket (🎯T2.10). The daemon admits
// the run against the plan-usage snapshot `claudia broker usage` prints,
// spawns the provider, and streams task_event until task_done. This command
// does not construct an in-process Task.

func taskCmd(args []string) error {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: claudia broker task [--provider P] [--model M] [--workdir DIR] [--id ID] [--timeout D] [--json] [--prompt TEXT | TEXT...]\n")
		fs.PrintDefaults()
	}
	provider := fs.String("provider", "claude", "provider: claude, codex, grok, cursor, bedrock, ollama")
	model := fs.String("model", "", "model override")
	workdir := fs.String("workdir", ".", "working directory the task runs in")
	id := fs.String("id", "", "task id")
	timeout := fs.Duration("timeout", defaultSeatTimeout, "bound for the whole run (0 waits until task_done)")
	asJSON := fs.Bool("json", false, "print task_started, task_event, and task_done as NDJSON")
	promptFlag := fs.String("prompt", "", "the turn; otherwise the remaining arguments are joined")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	prompt := strings.TrimSpace(*promptFlag)
	switch {
	case prompt != "" && fs.NArg() > 0:
		return errors.New("task: pass the prompt with --prompt or as arguments, not both")
	case prompt == "":
		prompt = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if prompt == "" {
		return errors.New("task: prompt required")
	}
	if *timeout < 0 {
		return errors.New("task: timeout must not be negative")
	}
	cfg := claudia.TaskConfig{
		ID:       *id,
		Provider: claudia.Provider(*provider),
		Model:    *model,
		WorkDir:  *workdir,
	}
	raw, err := claudia.EncodeTaskConfigWire(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c, err := dialConn()
	if err != nil {
		return err
	}
	defer c.Close()
	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()

	var deadline time.Time
	if *timeout > 0 {
		deadline = time.Now().Add(*timeout)
	}
	s := &cliConn{c: c}
	resp, pushes, err := s.call(ctx, &broker.Request{
		Type:    broker.TypeTaskRun,
		TaskRun: &broker.TaskRunRequest{Task: raw, Prompt: prompt},
	}, deadline)
	if err != nil {
		return taskWireError(err)
	}
	if resp.TaskStarted == nil {
		return fmt.Errorf("broker: task_run answered with %s", resp.Type)
	}
	var printed strings.Builder
	_, failed, err := emitTaskResponses(append([]*broker.Response{resp}, pushes...), *asJSON, &printed)
	if err != nil {
		return err
	}
	return readTaskStream(ctx, s, deadline, *asJSON, failed, &printed)
}

// taskWireError turns a plan_exhausted refusal into [claudia.ErrPlanExhausted].
func taskWireError(err error) error {
	var pe *broker.ProtocolError
	if errors.As(err, &pe) && pe.Code == broker.CodePlanExhausted {
		return fmt.Errorf("%w: %s", claudia.ErrPlanExhausted, pe.Error())
	}
	return err
}

// readTaskStream reads task_event pushes until task_done.
func readTaskStream(ctx context.Context, s *cliConn, deadline time.Time, asJSON bool, failed bool, printed *strings.Builder) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.c.SetDeadline(deadline); err != nil {
			return err
		}
		resp, err := s.c.ReadResponse()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isDeadline(err) {
				return errors.New("timed out waiting for task_done")
			}
			return err
		}
		done, bad, err := emitTaskResponses([]*broker.Response{resp}, asJSON, printed)
		if err != nil {
			return err
		}
		failed = failed || bad
		if done {
			if failed {
				return errors.New("task failed")
			}
			return nil
		}
	}
}

// emitTaskResponses prints one broker response. done is set on task_done.
// bad is set when a task_event or task_done carries a failure. printed is
// the text already written in human mode.
func emitTaskResponses(rs []*broker.Response, asJSON bool, printed *strings.Builder) (done bool, bad bool, err error) {
	for _, resp := range rs {
		switch resp.Type {
		case broker.TypeTaskStarted, broker.TypeTaskEvent, broker.TypeTaskDone:
			if asJSON {
				line, err := resp.Encode()
				if err != nil {
					return false, false, err
				}
				if _, err := fmt.Printf("%s\n", line); err != nil {
					return false, false, err
				}
			}
		}
		switch resp.Type {
		case broker.TypeTaskEvent:
			ev, err := claudia.DecodeTaskEventWire(resp.TaskEvent.Event)
			if err != nil {
				return false, false, err
			}
			if !asJSON {
				printTaskEvent(ev, printed)
			}
			if ev.Type == claudia.TaskEventError || ev.IsError {
				bad = true
			}
		case broker.TypeTaskDone:
			done = true
			if resp.TaskDone != nil && resp.TaskDone.Error != "" {
				bad = true
				if !asJSON {
					fmt.Fprintln(os.Stderr, resp.TaskDone.Error)
				}
			}
		case broker.TypeError:
			return false, false, taskWireError(resp.Error.Err())
		}
	}
	return done, bad, nil
}

// printTaskEvent writes the turn's text. A result that repeats the text
// already printed is a newline, so a one-line answer is one line.
func printTaskEvent(ev claudia.TaskEvent, printed *strings.Builder) {
	switch ev.Type {
	case claudia.TaskEventText:
		fmt.Print(ev.Content)
		printed.WriteString(ev.Content)
	case claudia.TaskEventResult:
		if ev.Content != "" && ev.Content != printed.String() {
			if printed.Len() > 0 && !strings.HasSuffix(printed.String(), "\n") {
				fmt.Println()
			}
			fmt.Println(ev.Content)
			return
		}
		if printed.Len() > 0 && !strings.HasSuffix(printed.String(), "\n") {
			fmt.Println()
		}
	case claudia.TaskEventError:
		if ev.ErrorMsg != "" {
			fmt.Fprintln(os.Stderr, ev.ErrorMsg)
		}
	}
}
