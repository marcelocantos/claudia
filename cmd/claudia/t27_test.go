// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/daemon"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestSupervisorInstallRendersProgram(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join("..", "..", "supervisor", "install.sh")
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(),
		"SUPERVISOR_SKIP_CTL=1",
		"SUPERVISOR_CONF_DIR="+dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dir, "claudia.ini"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"[program:claudia]",
		"supervisor/run-claudia.sh",
		"TERM=\"xterm-256color\"",
		"LANG=\"en_US.UTF-8\"",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("rendered claudia.ini missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "@REPO@") {
		t.Fatalf("rendered claudia.ini still has @REPO@:\n%s", s)
	}
}

func TestSupervisorInstallPinsClaudiaBin(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join("..", "..", "supervisor", "install.sh")
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(),
		"SUPERVISOR_SKIP_CTL=1",
		"SUPERVISOR_CONF_DIR="+dir,
		"CLAUDIA_BIN=/tmp/claudia-dev",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dir, "claudia.ini"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `CLAUDIA_BIN="/tmp/claudia-dev"`) {
		t.Fatalf("CLAUDIA_BIN not pinned:\n%s", body)
	}
}

func TestSupervisorInstallPreservesExistingPin(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "claudia.ini")
	if err := os.WriteFile(dest, []byte(`environment=HOME="/tmp",LANG="en_US.UTF-8",CLAUDIA_BIN="/tmp/kept"`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("..", "..", "supervisor", "install.sh")
	cmd := exec.Command(script)
	env := []string{"SUPERVISOR_SKIP_CTL=1", "SUPERVISOR_CONF_DIR=" + dir}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "CLAUDIA_BIN=") || strings.HasPrefix(e, "SUPERVISOR_SKIP_CTL=") || strings.HasPrefix(e, "SUPERVISOR_CONF_DIR=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `CLAUDIA_BIN="/tmp/kept"`) {
		t.Fatalf("existing CLAUDIA_BIN pin not preserved:\n%s\n%s", body, out)
	}
}

func TestSupervisorRunScriptPrefersHomebrewOpt(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "supervisor", "run-claudia.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"opt/claudia/bin/claudia",
		"broker serve",
		"CLAUDIA_BIN",
		"TERM=",
		"LANG=",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("run-claudia.sh missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "command -v claudia") || strings.Contains(s, "LookPath") {
		t.Fatal("run-claudia.sh must not resolve claudia from PATH (~/go/bin shadows brew)")
	}
}

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
		"std_service_path_env",
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

func startCLIDaemon(t *testing.T, usage []claudia.PlanUsage) (string, *daemon.Daemon) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "b.sock")
	d, err := daemon.New(daemon.Options{
		SocketPath:    sock,
		StateDir:      filepath.Join(dir, "state"),
		DisableResume: true,
		DisableIntel:  true,
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
	return captureFD(t, &os.Stdout, fn)
}

func captureStderr(t *testing.T, fn func() error) string {
	t.Helper()
	return captureFD(t, &os.Stderr, fn)
}

func captureFD(t *testing.T, fd **os.File, fn func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := *fd
	*fd = w
	fnErr := fn()
	_ = w.Close()
	*fd = old
	body, _ := io.ReadAll(r)
	if fnErr != nil {
		t.Fatal(fnErr)
	}
	return string(body)
}

func ptr[T any](v T) *T { return &v }
