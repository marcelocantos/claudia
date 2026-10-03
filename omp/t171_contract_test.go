// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestT171ExplicitBinaryAndActivation(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claudia")
	if err := os.WriteFile(bin, []byte("artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "claudia", bin, bin + "-missing", filepath.Dir(bin)} {
		if _, err := explicitLiveBinary(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	if err := os.Chmod(bin, 0700); err != nil {
		t.Fatal(err)
	}
	bin, err := explicitLiveBinary(bin)
	if err != nil {
		t.Fatal(err)
	}
	a := liveActivation{BrokerBinary: bin, BrokerSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("artifact"))), BrokerSocket: "/broker.sock", SidecarSocket: "/sidecar.sock", BrokerPID: 12, SidecarPID: 13, BrokerStart: "start1", SidecarStart: "start2", LoadedArtifactEvidence: "operator receipt tied to immutable tree and startup log"}
	if err := a.validate(bin, a.BrokerSocket, a.SidecarSocket); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*liveActivation){
		func(a *liveActivation) { a.BrokerPID = 0 },
		func(a *liveActivation) { a.SidecarPID = a.BrokerPID },
		func(a *liveActivation) { a.LoadedArtifactEvidence = "" },
		func(a *liveActivation) { a.BrokerSHA256 = "wrong" },
		func(a *liveActivation) { a.BrokerBinary = "/other" },
		func(a *liveActivation) { a.SidecarStart = "" },
	} {
		bad := a
		mutate(&bad)
		if err := bad.validate(bin, a.BrokerSocket, a.SidecarSocket); err == nil {
			t.Fatal("accepted incomplete/mismatched identity")
		}
	}
	if err := a.validate(bin, "", a.SidecarSocket); err == nil {
		t.Fatal("default socket accepted")
	}
	if hasLiveSocket([]byte("n/broker.sock.old\n"), "/broker.sock") {
		t.Fatal("socket prefix accepted")
	}
	if !hasLiveSocket([]byte("p12\nn/broker.sock\n"), "/broker.sock") {
		t.Fatal("exact socket rejected")
	}
}

func TestT171SupportedRecoveryCommandContract(t *testing.T) {
	for _, initial := range []string{HealthOK, HealthRejected, HealthExpired} {
		t.Run(initial, func(t *testing.T) {
			var calls [][]string
			before, after, err := recoverLivePlan(context.Background(), func(ctx context.Context, args ...string) ([]byte, error) {
				if ctx == nil {
					t.Fatal("missing bound context")
				}
				calls = append(calls, args)
				state := initial
				if len(calls) == 3 {
					state = HealthOK
				}
				return []byte(fmt.Sprintf(`{"plans":[{"provider":"anthropic","state":%q}]}`, state)), nil
			}, Anthropic)
			if err != nil || before != initial || after != HealthOK {
				t.Fatalf("%s -> %s: %v", before, after, err)
			}
			want := [][]string{{"broker", "auth-status", "--json"}, {"broker", "auth-recover", "--no-login", Anthropic}, {"broker", "auth-status", "--json"}}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("commands: %v", calls)
			}
		})
	}
	calls := 0
	_, _, err := recoverLivePlan(context.Background(), func(context.Context, ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"plans":[{"provider":"anthropic","state":"rejected"}]}`), nil
		}
		return nil, errors.New("needs sign-in")
	}, Anthropic)
	if err == nil || calls != 2 {
		t.Fatal("refusal must not retry or sign in")
	}
}

func TestT171RecoveryRenewsExpiredPlan(t *testing.T) {
	store := t165Store(t, "expired", time.Now().Add(-time.Hour))
	var verbs []string
	login := t165Login(&verbs, func() ([]byte, error) {
		return []byte(`{"refresh_token":"r2","access_token":"renewed","expiry":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
	})
	login.NoLogin = true
	if err := RecoverPlan(context.Background(), store, login, Anthropic); err != nil {
		t.Fatal(err)
	}
	item, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Records[Anthropic].AccessToken != "renewed" || !reflect.DeepEqual(verbs, []string{"refresh"}) {
		t.Fatal("recovery did not renew expired token exactly once")
	}
	if _, err := os.Stat(store.DataPath); err != nil {
		t.Fatalf("recovered plan not persisted: %v", err)
	}
	if err := RecoverPlan(context.Background(), store, login, Anthropic); err != nil {
		t.Fatal(err)
	}
	if len(verbs) != 1 {
		t.Fatal("healthy recovery rotated token again")
	}
}

func TestT171BounceRequiresAuthorizationAndReadiness(t *testing.T) {
	env := map[string]string{"CLAUDIA_OMP_BOUNCE_ARGV": `["/bin/restart","jevonsd"]`, "CLAUDIA_OMP_READY_ARGV": `["/bin/ready","jevonsd"]`}
	get := func(k string) string { return env[k] }
	if _, _, err := bounceCommands(get); err == nil {
		t.Fatal("unauthorized bounce accepted")
	}
	env["CLAUDIA_OMP_BOUNCE_AUTHORIZED"] = "restart-jevonsd"
	restart, ready, err := bounceCommands(get)
	if err != nil || len(restart) != 2 || len(ready) != 2 {
		t.Fatal("explicit argv rejected", err)
	}
	env["CLAUDIA_OMP_READY_ARGV"] = `[]`
	if _, _, err := bounceCommands(get); err == nil {
		t.Fatal("missing readiness accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := waitLiveReady(ctx, func(context.Context) bool { return false }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded readiness: %v", err)
	}
}

func TestT171SmokeCommandContract(t *testing.T) {
	for _, mode := range []string{"steer", "submit", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			var verbs []string
			err := smokeLiveSeat(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
				if len(args) < 2 || args[0] != "broker" {
					t.Fatal("unsupported command", args)
				}
				verbs = append(verbs, args[1])
				switch len(verbs) {
				case 1:
					return []byte(`{"grant":{"name":"test-seat"},"text":"T171_SMOKE_OK"}`), nil
				case 2:
					return nil, nil
				case 3:
					if !reflect.DeepEqual(args[:4], []string{"broker", "send", "--mode", "steer"}) {
						t.Fatal("wrong steer command")
					}
					return []byte(fmt.Sprintf(`{"sent":{"mode":%q}}`, mode)), nil
				case 4:
					if !reflect.DeepEqual(args, []string{"broker", "interrupt", "--timeout", "30s", "test-seat"}) {
						t.Fatal("wrong interrupt command")
					}
					return nil, nil
				}
				return nil, errors.New("unexpected command")
			}, "test-seat", "cursor", "/tmp/test-work")
			if mode == "steer" {
				if err != nil || !reflect.DeepEqual(verbs, []string{"grant", "send", "send", "interrupt"}) {
					t.Fatalf("smoke: %v; %v", err, verbs)
				}
			} else if err == nil {
				t.Fatal("wrong send mode accepted")
			}
		})
	}
}
