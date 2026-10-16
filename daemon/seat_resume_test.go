// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

// No prompt is sent: this exercises real load/adopt/store admission through
// both SDK and daemon, with fake credentials and no provider request.
func TestSeatResumeActualSidecarDirectAndBroker(t *testing.T) {
	f := newFixture(t)
	root := filepath.Dir(f.sock)
	t.Setenv("HOME", root)
	socket := filepath.Join(root, "omp.sock")
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bun, "../sidecar/server.ts", socket)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "JEVONS_SPOOL_DIR=" + filepath.Join(root, "spool")}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ctx := t.Context()
	for !omp.Listening(ctx, socket) {
		select {
		case <-ctx.Done():
			t.Fatalf("sidecar: %v", ctx.Err())
		// 🎯T97 exemption: expiry only paces another readiness probe; it
		// cannot pass or fail an assertion. go test -timeout bounds the wait.
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Setenv(omp.SocketEnv, socket)
	t.Setenv("JEVONS_SPOOL_DIR", filepath.Join(root, "spool"))
	t.Setenv("CLAUDIA_OMP_NO_REFRESH", "1")
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	item := omp.Item{Records: map[string]omp.Record{}}
	for _, p := range []string{omp.Anthropic, omp.OpenAICodex, omp.XAIOAuth, omp.Cursor} {
		item.Records[p] = omp.Record{AccessToken: "fixture", RefreshToken: "fixture", Expiry: time.Now().Add(time.Hour)}
	}
	raw, _ := json.Marshal(item)
	if err := omp.Open(ctx, omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) { return raw, nil }}); err != nil {
		t.Fatal(err)
	}
	opts := f.options(nil)
	opts.DisableResume = true
	opts.launchers = &claudia.RegistryLaunchers{Start: claudia.StartDirectContext, Adopt: claudia.Adopt}
	f.bootWith(t, opts)
	drop := func(name string) {
		t.Helper()
		c, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if err := json.NewEncoder(c).Encode(omp.Message{Op: "drop", Seat: name, SessionID: "generation"}); err != nil {
			t.Fatal(err)
		}
		var ev omp.Event
		if err := json.NewDecoder(c).Decode(&ev); err != nil || ev.Type != "dropped" {
			t.Fatalf("drop: %+v %v", ev, err)
		}
	}
	providers := []claudia.Provider{"", claudia.ProviderClaude, "anthropic", claudia.ProviderCodex, "openai-codex", claudia.ProviderGrok, "xai-oauth", claudia.ProviderCursor}
	for _, direct := range []bool{true, false} {
		for i, p := range providers {
			t.Run(fmt.Sprintf("direct=%v/provider=%s", direct, p), func(t *testing.T) {
				name := fmt.Sprintf("s-%v-%d", direct, i)
				path := filepath.Join(root, fmt.Sprintf("registry-%s.json", name))
				reg, err := claudia.NewRegistry(path)
				if err != nil {
					t.Fatal(err)
				}
				reg.SetDirect(direct)
				def := claudia.AgentDef{Name: name, SessionID: "generation", Provider: p, WorkDir: root, TermLogPath: "-"}
				if err := reg.Register(def); err != nil {
					t.Fatal(err)
				}
				a, err := reg.LaunchContext(ctx, name)
				if err != nil {
					t.Fatal("fresh launch:", err)
				}
				if a.WindowID() != "" || a.AttachCommand() != "" {
					t.Fatal("launched native transport")
				}
				if p == "" && a.Provider() != claudia.ProviderClaude {
					t.Fatalf("default provider=%s", a.Provider())
				}
				for _, op := range []string{omp.OpLoad, omp.OpAdopt} {
					c, err := net.Dial("unix", socket)
					if err != nil {
						t.Fatal(err)
					}
					_ = c.SetDeadline(time.Now().Add(5 * time.Second))
					if err := json.NewEncoder(c).Encode(omp.Message{Op: op, Seat: name, RequireResume: true, Token: "fixture"}); err != nil {
						c.Close()
						t.Fatal(err)
					}
					var ev omp.Event
					err = json.NewDecoder(c).Decode(&ev)
					c.Close()
					if err != nil || ev.Type != "error" || ev.Reason != "seat_identity_mismatch" || ev.ResumeChecked {
						t.Fatalf("required %s without generation: %+v %v", op, ev, err)
					}
				}
				// A matching already-loaded generation may be adopted even before its
				// first turn; this is not permission to recreate it after it disappears.
				adopted, err := claudia.Adopt(claudia.Config{Name: name, SessionID: "generation", Provider: p, WorkDir: root, TermLogPath: "-"})
				if err != nil {
					t.Fatal("adopt:", err)
				}
				_ = adopted // The explicit fixture drop below unloads this seat.
				if _, err := claudia.Adopt(claudia.Config{Name: name, SessionID: "other", Provider: p, WorkDir: root, TermLogPath: "-"}); err == nil {
					t.Fatal("foreign generation adopted")
				}
				if err := reg.MarkMaterialized(name); err != nil {
					t.Fatal("host materialization:", err)
				}
				reg.Stop(name)
				drop(name)
				loaded, err := claudia.NewRegistry(path)
				if err != nil {
					t.Fatal(err)
				}
				loaded.SetDirect(direct)
				if _, err := loaded.LaunchContext(ctx, name); err == nil {
					t.Fatal("empty persisted generation reminted")
				}
				store := filepath.Join(root, "omp-seats", name, "generation.jsonl")
				if err := os.MkdirAll(filepath.Dir(store), 0700); err != nil {
					t.Fatal(err)
				}
				header := fmt.Sprintf("{\"type\":\"claudia_seat\",\"v\":1,\"seat\":%q,\"session\":\"generation\"}\n", name)
				body := header + `{"type":"message","id":"user1","parentId":null,"timestamp":"2026-10-16T00:00:00Z","message":{"role":"user","content":"preserved","timestamp":1}}` + "\n"
				if err := os.WriteFile(store, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				a, err = loaded.LaunchContext(ctx, name)
				if err != nil {
					t.Fatal("durable generation:", err)
				}
				loaded.Stop(name)
				drop(name)
				// Poison the former admission source with another generation's
				// conversation. Seat-wide spool history must confer no authority.
				spoolDir := filepath.Join(root, "spool")
				if err := os.MkdirAll(spoolDir, 0700); err != nil {
					t.Fatal(err)
				}
				oldSpool := fmt.Sprintf("{\"ts\":\"2026-10-16T00:00:00Z\",\"seat\":%q,\"type\":\"text\",\"text\":\"old generation\"}\n", name)
				if err := os.WriteFile(filepath.Join(spoolDir, "events-2026-10-16.log"), []byte(oldSpool), 0600); err != nil {
					t.Fatal(err)
				}
				// A different generation's valid store cannot authorize this one.
				other := def
				other.SessionID = "other"
				other.Materialized = true
				if err := loaded.Register(other); err != nil {
					t.Fatal(err)
				}
				if _, err := loaded.LaunchContext(ctx, name); err == nil {
					t.Fatal("old generation authorized new generation")
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(store), "other.jsonl")); !os.IsNotExist(err) {
					t.Fatalf("refused load wrote new store: %v", err)
				}
				if err := loaded.Register(def); err != nil {
					t.Fatal(err)
				}
				// A persisted false Materialized flag still requires the old generation.
				again, err := claudia.NewRegistry(path)
				if err != nil {
					t.Fatal(err)
				}
				again.SetDirect(direct)
				broken := body + "{torn"
				if err := os.WriteFile(store, []byte(broken), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := again.LaunchContext(ctx, name); err == nil {
					t.Fatal("corrupt generation loaded")
				}
				after, err := os.ReadFile(store)
				if err != nil || string(after) != broken {
					t.Fatal("refused load repaired or changed history")
				}
			})
		}
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	native := claudia.SessionJSONLPath("native-history", canonicalRoot)
	if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
		t.Fatal(err)
	}
	nativeBytes := []byte(`{"type":"user","message":{"role":"user","content":"native conversation"}}` + "\n")
	if err := os.WriteFile(native, nativeBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := claudia.StartDirectContext(ctx, claudia.Config{Name: "native", SessionID: "native-history", WorkDir: root, TermLogPath: "-"}); err == nil {
		t.Fatal("native CLI conversation silently replaced")
	}
	after, err := os.ReadFile(native)
	if err != nil || string(after) != string(nativeBytes) {
		t.Fatal("native transcript changed")
	}

}
