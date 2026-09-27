// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestBrokerRunSelectsAndStreamsRestrictedTask(t *testing.T) {
	remaining := 60.0
	sock, _ := startCLIDaemon(t, []claudia.PlanUsage{{
		Provider: claudia.ProviderGrok, Status: claudia.PlanUsageAvailable,
		Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &remaining}},
	}})
	t.Setenv(broker.SocketPathEnv, sock)
	t.Setenv(broker.NoBrokerEnv, "")
	if _, err := roundTrip(&broker.Request{Type: broker.TypeUsage, Usage: &broker.UsageRequest{Refresh: true}}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fake-grok")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"text\",\"data\":\"healthy\"}' '{\"type\":\"end\",\"stopReason\":\"EndTurn\",\"sessionId\":\"test-session\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BIN", bin)
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgRaw, err := claudia.EncodeTaskConfigWire(claudia.TaskConfig{
		WorkDir: home, ToolPolicy: &claudia.TaskToolPolicy{Builtins: []string{"read_file"}, MaxTurns: 2, HomeDir: home},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(map[string]any{
		"predicates": json.RawMessage(`{"quality":"standard"}`),
		"tasks":      map[string]json.RawMessage{"grok": cfgRaw},
		"prompt":     "inspect",
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBrokerRunClient$")
	cmd.Env = append(os.Environ(), "CLAUDIA_BROKER_RUN_CHILD=1", "CLAUDIA_NO_BROKER=0")
	cmd.Stdin = bytes.NewReader(request)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("broker run client: %v", err)
	}
	var sawResult bool
	lines := bytes.Split(bytes.TrimSpace(output), []byte{'\n'})
	var selection struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal(lines[0], &selection); err != nil || selection.Type != "selection" || selection.Provider != "grok" || selection.Model != "grok-4.5" {
		t.Fatalf("broker selection = %+v (%v)", selection, err)
	}
	for _, line := range lines[1:] {
		ev, err := claudia.DecodeTaskEventWire(line)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == claudia.TaskEventResult && ev.Content == "healthy" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("no health result in %s", output)
	}
}

func TestBrokerRunClient(t *testing.T) {
	if os.Getenv("CLAUDIA_BROKER_RUN_CHILD") != "1" {
		return
	}
	if err := brokerRunCmd(nil, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestBrokerRunRejectsIncompleteInput(t *testing.T) {
	for _, input := range []string{`{}`, `{"prompt":"hi"}`, `{"prompt":"hi","tasks":{"grok":{}}} {}`} {
		var output bytes.Buffer
		if err := brokerRunCmd(nil, strings.NewReader(input), &output); err == nil {
			t.Errorf("accepted %s", input)
		}
		if !strings.Contains(output.String(), `"type":"error"`) {
			t.Errorf("error was not emitted as a JSONL event: %s", output.String())
		}
	}
}
