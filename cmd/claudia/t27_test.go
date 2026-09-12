// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestHomebrewFormulaStartsBrokerServe(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "tapper", "formula_includes.rb"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"service do",
		`opt_bin/"claudia"`,
		`"broker"`,
		`"serve"`,
		"keep_alive true",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("tapper/formula_includes.rb missing %q:\n%s", want, s)
		}
	}
}

func TestBrokerStatusPrintsGrantsAndBands(t *testing.T) {
	sock, _ := startCLIDaemon(t, []claudia.PlanUsage{{
		Provider: claudia.ProviderClaude,
		Status:   claudia.PlanUsageAvailable,
		Windows: []claudia.PlanWindow{{
			Name:             claudia.PlanWindowWeekly,
			RemainingPercent: ptr(40.0),
		}},
	}})
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	if _, err := roundTrip(&broker.Request{Type: broker.TypeUsage, Usage: &broker.UsageRequest{Refresh: true}}); err != nil {
		t.Fatal(err)
	}

	var out string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out = captureStdout(t, status)
		if strings.Contains(out, "claude=") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "protocol v") {
		t.Fatalf("status missing protocol: %q", out)
	}
	if !strings.Contains(out, "grants 0") {
		t.Fatalf("status missing grant count: %q", out)
	}
	if !strings.Contains(out, "tasks 0") {
		t.Fatalf("status missing task count: %q", out)
	}
	if !strings.Contains(out, "claude=") {
		t.Fatalf("status missing plan band: %q", out)
	}
	if !strings.Contains(out, "no seats") {
		t.Fatalf("status missing empty grants: %q", out)
	}
}

func TestBrokerTailStreamsNDJSON(t *testing.T) {
	sock, d := startCLIDaemon(t, []claudia.PlanUsage{{
		Provider: claudia.ProviderGrok,
		Status:   claudia.PlanUsageAvailable,
	}})
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan error, 1)
	go func() { done <- tail() }()
	time.Sleep(50 * time.Millisecond)
	if _, err := roundTrip(&broker.Request{Type: broker.TypeUsage, Usage: &broker.UsageRequest{Refresh: true}}); err != nil {
		os.Stdout = old
		_ = w.Close()
		t.Fatal(err)
	}

	dec := json.NewDecoder(bufio.NewReader(r))
	var ev broker.EventMessage
	got := make(chan error, 1)
	go func() { got <- dec.Decode(&ev) }()

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("tail NDJSON: %v", err)
		}
	case err := <-done:
		t.Fatalf("tail exited before an event: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tail NDJSON")
	}
	if ev.Kind == "" {
		t.Fatalf("empty tail event: %+v", ev)
	}

	_ = d.Close()
	_ = w.Close()
	os.Stdout = old
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func startCLIDaemon(t *testing.T, usage []claudia.PlanUsage) (string, *claudia.BrokerDaemon) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	d, err := claudia.NewBrokerDaemon(claudia.BrokerDaemonOptions{
		SocketPath:    sock,
		StateDir:      filepath.Join(dir, "state"),
		DisableResume: true,
		UsageTTL:      time.Hour,
		UsageFetch: func(context.Context) ([]claudia.PlanUsage, error) {
			return usage, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return sock, d
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fnErr := fn()
	_ = w.Close()
	os.Stdout = old
	body, _ := io.ReadAll(r)
	if fnErr != nil {
		t.Fatal(fnErr)
	}
	return string(body)
}

func ptr[T any](v T) *T { return &v }
