// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package claudia_test provides runnable examples for the claudia package.
package claudia_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/marcelocantos/claudia"
)

// ExampleRun demonstrates Task mode: send a single prompt and receive the
// response. This is the simplest usage — no session persistence, no event
// streaming.
func ExampleRun() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := claudia.Run(ctx, "Respond with: hello", claudia.Config{
		WorkDir: os.TempDir(),
		Model:   "haiku",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result)
}

// ExampleNewTask demonstrates Task mode with explicit event streaming.
// Collect each event type as it arrives, then read the final result.
func ExampleNewTask() {
	task := claudia.NewTask(claudia.TaskConfig{
		ID:      "example-task",
		Name:    "example",
		WorkDir: os.TempDir(),
		Model:   "haiku",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	events, err := task.Run(ctx, "Respond with: hello")
	if err != nil {
		log.Fatal(err)
	}

	for ev := range events {
		switch ev.Type {
		case claudia.TaskEventText:
			fmt.Print(ev.Content)
		case claudia.TaskEventResult:
			fmt.Printf("\ncost: $%.4f  tokens: %d in / %d out\n",
				ev.CostUSD, ev.Usage.InputTokens, ev.Usage.OutputTokens)
		case claudia.TaskEventError:
			log.Println("error:", ev.ErrorMsg)
		}
	}

	// Preserve the Claude session ID to resume conversation later.
	_ = task.ClaudeID()
}

// ExampleStart demonstrates Session mode: a persistent agent that survives
// consumer restarts. Use WaitForResponse to block until the assistant replies.
func ExampleStart() {
	agent, err := claudia.Start(claudia.Config{
		WorkDir: os.TempDir(),
		Model:   "haiku",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer agent.Stop()

	// Observe every JSONL event (optional — remove if not needed).
	token := agent.SubscribeEvents(func(ev claudia.Event) {
		if ev.Type == "assistant" && ev.Text != "" {
			fmt.Println("delta:", ev.Text)
		}
	})
	defer agent.UnsubscribeEvents(token)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := agent.Send("Respond with: hello"); err != nil {
		log.Fatal(err)
	}

	reply, err := agent.WaitForResponse(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("reply:", reply)

	// The session ID can be stored and passed to Config.SessionID to
	// resume this conversation in a future process.
	fmt.Println("session:", agent.SessionID())
}

// ExampleAcquire demonstrates the warm agent pool. Acquire checks out a
// pre-warmed agent; Release returns it for the next caller.
func ExampleAcquire() {
	cfg := claudia.Config{
		WorkDir:    os.TempDir(),
		Model:      "haiku",
		PoolPolicy: "spawn", // create a new window if none is idle
	}

	agent, err := claudia.Acquire(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := agent.Send("Respond with: hello"); err != nil {
		// Drop the window if send fails — it may be in a bad state.
		_ = agent.Release("drop")
		log.Fatal(err)
	}

	reply, err := agent.WaitForResponse(ctx)
	if err != nil {
		_ = agent.Release("drop")
		log.Fatal(err)
	}
	fmt.Println(reply)

	// Return the window to the pool so the next Acquire can reuse it.
	if err := agent.Release("return"); err != nil {
		log.Println("release:", err)
	}
}

// ExampleNewRegistry demonstrates the Registry: register named agents,
// launch them, and stop them all on shutdown.
func ExampleNewRegistry() {
	reg, err := claudia.NewRegistry("/tmp/claudia-agents.json")
	if err != nil {
		log.Fatal(err)
	}

	// EnsureAgent is idempotent — safe to call on every startup.
	def, err := reg.EnsureAgent("helper", os.TempDir(), "haiku", true)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("agent:", def.Name, "session:", def.SessionID)

	agent, err := reg.Launch("helper")
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := agent.Send("Respond with: hello"); err != nil {
		log.Fatal(err)
	}
	reply, err := agent.WaitForResponse(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(reply)

	reg.StopAll()
}
