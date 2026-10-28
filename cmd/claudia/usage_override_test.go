// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/controlplane"
	"github.com/marcelocantos/claudia/daemon"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestParseUsageOverrideProviders(t *testing.T) {
	got, err := parseUsageOverrideProviders(" grok, claude ")
	if err != nil || len(got) != 2 || got[0] != claudia.ProviderGrok || got[1] != claudia.ProviderClaude {
		t.Fatalf("got %v err %v", got, err)
	}
	if _, err := parseUsageOverrideProviders("nope"); err == nil {
		t.Fatal("unknown")
	}
	if _, err := parseUsageOverrideProviders("grok,grok"); err == nil {
		t.Fatal("duplicate")
	}
	if _, err := parseUsageOverrideProviders(" , "); err == nil {
		t.Fatal("empty")
	}
}

func TestT234OverrideRefusesUnsafeState(t *testing.T) {
	dir := fixtureTestDir(t)
	binary := fixtureBinary(t)
	defaultDir, err := broker.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(defaultDir, 0700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.Mkdir(bad, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", defaultDir, bad, link} {
		cmd := exec.Command(binary, "broker", "serve", "--usage-fixture-override=grok", "--state-dir", path)
		cmd.Env = overrideTestEnv(t)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("accepted unsafe path %q: %s", path, out)
		}
	}
	if _, err := os.Stat(filepath.Join(defaultDir, "logs")); !os.IsNotExist(err) {
		t.Fatalf("default state wrote log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bad, "logs")); !os.IsNotExist(err) {
		t.Fatalf("unsafe state wrote log: %v", err)
	}
}

func TestT234OverrideRefusesSupervisorAndEscapes(t *testing.T) {
	dir := fixtureTestDir(t)
	binary := fixtureBinary(t)
	outside := fixtureTestDir(t)
	prodSock, err := productionBrokerSocket()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{
			name: "fixture combo",
			args: []string{"broker", "serve", "--test-usage-fixture", "--usage-fixture-override=grok", "--state-dir", dir},
			want: "usage override refuses --test-usage-fixture",
		},
		{
			name: "supervisor",
			env:  []string{"SUPERVISOR_ENABLED=1"},
			args: []string{"broker", "serve", "--usage-fixture-override=grok", "--state-dir", dir},
			want: "usage override refuses supervisor context",
		},
		{
			name: "mcp seed escape",
			args: []string{"broker", "serve", "--usage-fixture-override=grok", "--state-dir", dir, "--mcp-seed-dir", outside},
			want: "usage override refuses MCP seed outside private state",
		},
		{
			name: "consumer escape",
			args: []string{"broker", "serve", "--usage-fixture-override=grok", "--state-dir", dir, "--consumer-state-dir", outside},
			want: "usage override refuses consumer state escape",
		},
		{
			name: "production socket",
			args: []string{"broker", "serve", "--usage-fixture-override=grok", "--state-dir", dir, "--socket", prodSock},
			want: "usage override refuses production socket",
		},
		{
			name: "unknown provider",
			args: []string{"broker", "serve", "--usage-fixture-override=nope", "--state-dir", dir},
			want: "unknown provider",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binary, tc.args...)
			cmd.Env = append(overrideTestEnv(t), tc.env...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("accepted: %s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("got %q want substring %q", out, tc.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
				t.Fatalf("wrote logs after refusal: %v", err)
			}
		})
	}
}

func TestT234OverrideHTTPAllowlistAndReplica(t *testing.T) {
	t.Cleanup(claudia.ClearBrokerUsageOverride)
	dir := fixtureTestDir(t)
	if err := os.MkdirAll(filepath.Join(dir, overrideSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "broker.sock")
	var mu sync.Mutex
	var fetched []claudia.Provider
	d, err := daemon.New(daemon.Options{
		SocketPath:             sock,
		StateDir:               dir,
		DisableResume:          true,
		DisableIntel:           true,
		DisableMCPHost:         true,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		UsageOverrideProviders: []claudia.Provider{claudia.ProviderGrok},
		UsageProviderFetch: func(_ context.Context, p claudia.Provider) (claudia.PlanUsage, error) {
			mu.Lock()
			fetched = append(fetched, p)
			mu.Unlock()
			return claudia.PlanUsage{Provider: p, Status: claudia.PlanUsageUnavailable, Reason: "deterministic"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	closeOverride, err := startUsageOverride(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeOverride)

	replica, err := controlplane.OpenReplica(filepath.Join(t.TempDir(), "consumer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replica.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = replica.Link(ctx, claudia.ControlSocketPath(sock)) }()

	reset := time.Now().UTC().Add(7 * 24 * time.Hour * 9 / 10)
	grok := fixtureWeekly(claudia.ProviderGrok, 90, 10, reset)
	secret, err := os.ReadFile(filepath.Join(dir, overrideSubdir, overrideCapability))
	if err != nil {
		t.Fatal(err)
	}
	if code := overridePost(t, dir, []claudia.PlanUsage{grok}, "invalid"); code != 403 {
		t.Fatalf("unauthorized: %d", code)
	}
	claude := fixtureWeekly(claudia.ProviderClaude, 0, 100, reset)
	if code := overridePost(t, dir, []claudia.PlanUsage{claude}, string(secret)); code != 400 {
		t.Fatalf("unenrolled: %d", code)
	}
	if code := overridePost(t, dir, []claudia.PlanUsage{grok}, string(secret)); code != 200 {
		t.Fatalf("grok push: %d", code)
	}
	waitOverrideBand(t, replica, claudia.ProviderGrok, claudia.PlanBandOK, 90)
	mu.Lock()
	got := append([]claudia.Provider(nil), fetched...)
	mu.Unlock()
	for _, p := range got {
		if p == claudia.ProviderGrok {
			t.Fatalf("overridden provider reached real fetch: %v", got)
		}
	}
}

func overrideTestEnv(t *testing.T) []string {
	t.Helper()
	env := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, broker.SocketPathEnv+"=") || strings.HasPrefix(e, "SUPERVISOR_") {
			continue
		}
		env = append(env, e)
	}
	return env
}

func overridePost(t *testing.T, dir string, readings []claudia.PlanUsage, token string) int {
	t.Helper()
	body, err := json.Marshal(struct {
		Backends []claudia.PlanUsage `json:"backends"`
	}{readings})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, overrideSubdir, overrideSocketName))
	}}}
	req, _ := http.NewRequest("POST", "http://override/usage", bytes.NewReader(body))
	req.Header.Set("X-Fixture-Capability", token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func waitOverrideBand(t *testing.T, r *controlplane.Replica, provider claudia.Provider, band claudia.PlanBand, remaining float64) {
	t.Helper()
	updates, err := r.SubscribeQuery(t.Context(), "SELECT provider, status, success_at, reading_json FROM claudia_plan_usage")
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				t.Fatal("replica subscription closed")
			}
			if u, found := fixtureReadReplica(t, r)[provider]; found && len(u.Windows) > 0 && u.Windows[0].RemainingPercent != nil && *u.Windows[0].RemainingPercent == remaining {
				if u.Windows[0].Band != band {
					t.Fatalf("band=%s want %s", u.Windows[0].Band, band)
				}
				return
			}
		case <-t.Context().Done():
			t.Fatalf("replica did not deliver %s %s/%v: %v", provider, band, remaining, t.Context().Err())
		}
	}
}

func TestT234OverrideSidecarStillStarts(t *testing.T) {
	t.Cleanup(claudia.ClearBrokerUsageOverride)
	dir := fixtureTestDir(t)
	started := false
	d, err := daemon.New(daemon.Options{
		SocketPath:             filepath.Join(dir, "broker.sock"),
		StateDir:               dir,
		DisableResume:          true,
		DisableIntel:           true,
		DisableMCPHost:         true,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		UsageOverrideProviders: []claudia.Provider{claudia.ProviderGrok},
		UsageProviderFetch: func(_ context.Context, p claudia.Provider) (claudia.PlanUsage, error) {
			return claudia.PlanUsage{Provider: p, Status: claudia.PlanUsageUnavailable, Reason: "stub"}, nil
		},
		StartSidecar: func(context.Context) error {
			started = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if !started {
		t.Fatal("ordinary sidecar start skipped under usage override")
	}
}
