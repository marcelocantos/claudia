// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// This operator declaration binds the immutable artifacts to the already
// serving processes. It is not a way to install, trust, launch or repair them.
type usageLiveActivation struct {
	SourceDir, Commit, Tree, BrokerBinary, BrokerSHA256 string
	BrokerPID, SidecarPID                               int
	BrokerStart, SidecarStart                           string
	Root                                                string
	NoRefresh                                           bool
	Models                                              map[string]string
}

var usageLiveProviders = []string{omp.Anthropic, omp.OpenAICodex, omp.XAIOAuth, omp.Cursor}

func (a usageLiveActivation) validate(getenv func(string) string) error {
	if !filepath.IsAbs(a.Root) || filepath.Clean(a.Root) != a.Root || !strings.HasPrefix(filepath.Base(a.Root), "claudia-usage-live.") ||
		!filepath.IsAbs(a.SourceDir) || !filepath.IsAbs(a.BrokerBinary) || a.BrokerPID <= 1 || a.SidecarPID <= 1 || a.BrokerPID == a.SidecarPID ||
		a.BrokerStart == "" || a.SidecarStart == "" || len(a.Commit) != 40 || len(a.Tree) != 40 || len(a.BrokerSHA256) != 64 || !a.NoRefresh {
		return fmt.Errorf("OUTAGE: incomplete isolated accounting activation declaration")
	}
	for key, want := range map[string]string{
		"CLAUDIA_USAGE_ROOT": a.Root, "CLAUDIA_BROKER_SOCKET": filepath.Join(a.Root, "broker.sock"),
		omp.SocketEnv: filepath.Join(a.Root, "omp.sock"), "XDG_STATE_HOME": filepath.Join(a.Root, "state"),
		"CLAUDIA_OMP_SPOOL_DIR": filepath.Join(a.Root, "spool"), "CLAUDIA_OMP_SERVER": filepath.Join(a.SourceDir, "sidecar", "server.ts"),
		"CLAUDIA_NO_BROKER": "0", omp.NoNetworkEnv: "1",
	} {
		if getenv(key) != want {
			return fmt.Errorf("OUTAGE: %s must match the isolated activation declaration", key)
		}
	}
	for _, provider := range usageLiveProviders {
		if strings.TrimSpace(a.Models[provider]) == "" {
			return fmt.Errorf("OUTAGE: activation must declare an explicit %s model", provider)
		}
	}
	return nil
}

func usageLivePreflight(t *testing.T) usageLiveActivation {
	t.Helper()
	path := os.Getenv("CLAUDIA_USAGE_ACTIVATION")
	if !filepath.IsAbs(path) {
		t.Fatal("OUTAGE: set CLAUDIA_USAGE_ACTIVATION to an operator-prepared JSON declaration; this test never starts a broker")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var a usageLiveActivation
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if err := a.validate(os.Getenv); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{a.Root, a.SourceDir} {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || real != dir {
			t.Fatal("OUTAGE: activation directories must be canonical, existing paths")
		}
	}
	temp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.Root, temp+string(os.PathSeparator)) && !strings.HasPrefix(a.Root, "/private/tmp/") {
		t.Fatal("OUTAGE: accounting isolate must be under the system temporary directory")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	run := func(dir, name string, args ...string) string {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("OUTAGE: artifact/process preflight %s failed: %v", filepath.Base(name), err)
		}
		return strings.TrimSpace(string(out))
	}
	for _, dir := range []string{".", a.SourceDir} {
		if run(dir, "git", "rev-parse", "HEAD") != a.Commit || run(dir, "git", "rev-parse", "HEAD^{tree}") != a.Tree || run(dir, "git", "status", "--porcelain", "--untracked-files=normal") != "" {
			t.Fatal("OUTAGE: test and sidecar must use the declared clean source revision")
		}
	}
	bin, err := os.ReadFile(a.BrokerBinary)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(bin)) != a.BrokerSHA256 {
		t.Fatal("OUTAGE: broker binary differs from activation")
	}
	info, err := buildinfo.ReadFile(a.BrokerBinary)
	if err != nil {
		t.Fatal("OUTAGE: broker needs Go build provenance")
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["vcs.revision"] != a.Commit || settings["vcs.modified"] != "false" {
		t.Fatal("OUTAGE: broker was not built from the declared clean source revision")
	}
	for _, p := range []struct {
		pid           int
		start, socket string
	}{
		{a.BrokerPID, a.BrokerStart, os.Getenv("CLAUDIA_BROKER_SOCKET")}, {a.SidecarPID, a.SidecarStart, os.Getenv(omp.SocketEnv)},
	} {
		if run("", "/bin/ps", "-p", strconv.Itoa(p.pid), "-o", "lstart=") != p.start ||
			!strings.Contains("\n"+run("", "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(p.pid), "-U", "-Fn")+"\n", "\nn"+p.socket+"\n") || !omp.Listening(ctx, p.socket) {
			t.Fatal("OUTAGE: declared process does not own its listening socket")
		}
	}
	if run("", "/bin/ps", "-p", strconv.Itoa(a.BrokerPID), "-o", "comm=") != a.BrokerBinary {
		t.Fatal("OUTAGE: serving broker differs from declared artifact")
	}
	if run("", "/bin/ps", "-p", strconv.Itoa(a.SidecarPID), "-o", "ppid=") != strconv.Itoa(a.BrokerPID) {
		t.Fatal("OUTAGE: sidecar is not the declared broker's child")
	}
	command := run("", "/bin/ps", "-ww", "-p", strconv.Itoa(a.SidecarPID), "-o", "command=")
	wantArgs := " " + filepath.Join(a.SourceDir, "sidecar", "server.ts") + " " + os.Getenv(omp.SocketEnv) + " " + omp.LifelineArg
	if !strings.HasSuffix(command, wantArgs) {
		t.Fatal("OUTAGE: serving sidecar command does not name the declared source and socket")
	}
	if !BrokerAvailable() {
		t.Fatal("OUTAGE: activated broker is unavailable; never auto-start it")
	}
	t.Logf("operator activation: source=%s tree=%s broker_sha256=%s broker_pid=%d sidecar_pid=%d", a.Commit, a.Tree, a.BrokerSHA256, a.BrokerPID, a.SidecarPID)
	return a
}

// TestUsageAccountingLive checks the physical producer, live SDK projection,
// canonical recovery and broker reconnect with one owner input per provider.
// It does not force retry/compaction or claim a physical-call admission cap.
func TestUsageAccountingLive(t *testing.T) {
	if os.Getenv("CLAUDIA_USAGE_LIVE") == "" {
		t.Skip("CLAUDIA_USAGE_LIVE not set; spends four bounded subscription turns")
	}
	activation := usageLivePreflight(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	health, err := OMPAuthStatus(ctx)
	if err != nil {
		t.Fatal("OUTAGE: broker authentication status unavailable")
	}
	for _, provider := range usageLiveProviders {
		healthy := false
		for _, h := range health {
			healthy = healthy || string(h.Provider) == provider && h.State == omp.HealthOK
		}
		if !healthy {
			t.Fatalf("OUTAGE: %s needs external authentication recovery; this test never performs it", provider)
		}
	}
	for _, provider := range usageLiveProviders {
		t.Run(provider, func(t *testing.T) {
			turn, done := context.WithTimeout(ctx, 90*time.Second)
			defer done()
			name := "usage-live-" + newRunID()
			cfg := Config{Provider: Provider(provider), Model: activation.Models[provider], Name: name, WorkDir: t.TempDir(), SandboxMode: omp.SandboxReadOnly, MCPExclusive: true, TermLogPath: "-"}
			// Use the SDK's actual broker grant path without Start's intentional
			// direct fallback if the prepared broker disappears mid-preflight.
			a, err := startViaBrokerContext(turn, cfg)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			current := a
			defer func() { current.Stop() }()
			if !a.DaemonHeld() || !a.SupportsReceipts() {
				t.Fatal("current broker with durable receipts is required")
			}
			var mu sync.Mutex
			var events []Event
			terminal := make(chan struct{}, 1)
			unexpected := make(chan struct{}, 1)
			a.SubscribeEvents(func(e Event) {
				mu.Lock()
				events = append(events, e)
				mu.Unlock()
				if e.ProgressType == "tool_use" || e.Type == "usage_error" {
					select {
					case unexpected <- struct{}{}:
					default:
					}
				}
				if e.IsTerminalStop() {
					select {
					case terminal <- struct{}{}:
					default:
					}
				}
			})
			requestID := "usage-request-" + newRunID()
			if err := a.SendWithRequestID("Reply with exactly OK. Do not call any tools, read files, or perform other work.", requestID); err != nil {
				t.Fatalf("Send (never replayed): %v", err)
			}
			select {
			case <-unexpected:
				_ = a.Interrupt()
				t.Fatal("unexpected tool use or accounting error; stopped the slice")
			case <-turn.Done():
				_ = a.Interrupt()
				t.Fatal("live turn exceeded its bound")
			case <-terminal:
			}
			if reply, err := a.WaitForResponse(turn); err != nil || !strings.Contains(reply, "OK") {
				t.Fatalf("tiny no-tool turn failed: err=%v", err)
			}
			mu.Lock()
			observed := append([]Event(nil), events...)
			mu.Unlock()
			query := UsageQuery{Seat: name, SessionID: a.SessionID(), Limit: UsagePageLimit}
			read := func(direct bool) []UsageFact {
				t.Helper()
				var facts []UsageFact
				q := query
				for pages := 0; pages < 8; pages++ {
					var page *UsagePage
					var err error
					if direct {
						page, err = QueryUsageDirect(turn, q)
					} else {
						page, err = QueryUsage(turn, q)
					}
					if err != nil {
						t.Fatalf("canonical usage direct=%t: %v", direct, err)
					}
					facts = append(facts, page.Facts...)
					if !page.More {
						return facts
					}
					q.After = page.Next
				}
				t.Fatal("tiny turn exceeded bounded usage replay")
				return nil
			}
			facts := read(true)
			if len(facts) == 0 || !reflect.DeepEqual(facts, read(false)) {
				t.Fatal("direct/broker canonical facts differ or are absent")
			}
			latest := map[string]UsageFact{}
			for _, f := range facts {
				if err := f.ValidateIdentity(name, a.SessionID()); err != nil {
					t.Fatal(err)
				}
				if f.Coverage != "provider_attempt" || f.ProviderAttemptID != f.CallID || f.InvocationID == "" || f.Provider != provider || f.Kind != "model" || len(f.Requests) != 1 || f.Requests[0].RequestID != requestID || f.Requests[0].AttemptID == "" {
					t.Fatal("physical identity or original request attribution missing")
				}
				previous, ok := latest[f.CallID]
				var old *UsageFact
				if ok {
					old = &previous
				}
				apply, err := ValidateUsageTransition(old, &f)
				if err != nil {
					t.Fatal(err)
				}
				if apply {
					latest[f.CallID] = f
				}
			}
			var expected, eventSum Usage
			value := func(n *int64) int {
				if n == nil {
					return 0
				}
				return int(*n)
			}
			completed := 0
			invocations := map[string]bool{}
			measuredConsumption := false
			for _, f := range latest {
				invocations[f.InvocationID] = true
				// Wire-presence oracle for the actual OK response, not a billing
				// oracle. Missing cache dimensions and failed attempts stay unknown.
				measuredConsumption = measuredConsumption || f.State == "completed" &&
					(value(f.Counters.TotalInputTokens) > 0 || value(f.Counters.InputTokens) > 0) && value(f.Counters.OutputTokens) > 0
				if f.State == "started" {
					t.Fatal("terminal turn left an incomplete physical attempt")
				}
				if f.State == "completed" {
					completed++
				}
				if provider == omp.Cursor && f.Counters != (UsageCounters{}) {
					t.Fatal("Cursor protocol counters became billed tokens")
				}
				expected.InputTokens += value(f.Counters.InputTokens)
				expected.OutputTokens += value(f.Counters.OutputTokens)
				expected.CacheCreationInputTokens += value(f.Counters.CacheCreationInputTokens)
				expected.CacheReadInputTokens += value(f.Counters.CacheReadInputTokens)
				raw, _ := json.Marshal(f)
				t.Logf("physical usage (absent counters unknown, cost never quota): %s", raw)
			}
			if completed == 0 {
				t.Fatal("no completed physical attempt")
			}
			if len(invocations) != 1 || (provider != omp.Cursor && !measuredConsumption) {
				t.Fatal("tiny turn did not produce one measured model invocation (Cursor billing remains unknown)")
			}
			var eventFacts []UsageFact
			for _, e := range observed {
				if e.Type == "usage_error" || e.ProgressType == "tool_use" {
					t.Fatal("unexpected accounting failure or tool use")
				}
				if e.UsageFact != nil {
					eventFacts = append(eventFacts, *e.UsageFact)
				}
				eventSum.InputTokens += e.Usage.InputTokens
				eventSum.OutputTokens += e.Usage.OutputTokens
				eventSum.CacheCreationInputTokens += e.Usage.CacheCreationInputTokens
				eventSum.CacheReadInputTokens += e.Usage.CacheReadInputTokens
			}
			if !reflect.DeepEqual(facts, eventFacts) || a.Usage() != expected || eventSum != expected || a.UsageError() != nil {
				t.Fatal("live events, Agent.Usage and canonical known-counter projection differ")
			}
			for range 2 {
				if err := a.RefreshUsage(turn); err != nil || a.Usage() != expected {
					t.Fatalf("replay changed known usage: %v", err)
				}
			}
			if err := a.Detach(); err != nil {
				t.Fatal(err)
			}
			current, err = startViaBrokerContext(turn, cfg)
			if err != nil {
				current = a
				t.Fatal(err)
			}
			if current.SessionID() != query.SessionID {
				t.Fatal("reclaim changed generation")
			}
			if err := current.RefreshUsage(turn); err != nil || current.Usage() != expected || current.UsageError() != nil {
				t.Fatalf("broker reconnect lost or duplicated usage: %v", err)
			}
			if !reflect.DeepEqual(facts, read(true)) || !reflect.DeepEqual(facts, read(false)) {
				t.Fatal("reconnect/replay changed durable facts")
			}
		})
	}
}
