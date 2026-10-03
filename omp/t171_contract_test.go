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
	for _, outcome := range []string{RecoveryHealthyNoOp, RecoveryRefreshed, RecoveryFailure} {
		t.Run(outcome, func(t *testing.T) {
			calls := 0
			_, err := recoverLivePlan(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if !reflect.DeepEqual(args, []string{"broker", "auth-recover-detail", Anthropic}) {
					t.Fatalf("command %v", args)
				}
				class := "none"
				if outcome == RecoveryFailure {
					class = "needs_sign_in"
				}
				return []byte(fmt.Sprintf(`{"provider":"anthropic","outcome":%q,"classification":%q}`, outcome, class)), nil
			}, Anthropic)
			if calls != 1 {
				t.Fatal("recovery retried")
			}
			if outcome == RecoveryRefreshed && err != nil {
				t.Fatal(err)
			}
			if outcome == RecoveryHealthyNoOp && !errors.Is(err, errLiveRenewalEvidence) {
				t.Fatal("healthy no-op counted as renewal")
			}
			if outcome == RecoveryFailure && err == nil {
				t.Fatal("failed recovery counted as renewal")
			}
		})
	}
	calls := 0
	_, err := recoverLivePlan(context.Background(), func(context.Context, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("unsupported operation")
	}, Anthropic)
	if err == nil || calls != 1 {
		t.Fatal("unsupported detailed recovery fell back to old acknowledgement")
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
	if _, _, err := bounceCommands(get); !errors.Is(err, errLiveBouncePermit) {
		t.Fatal("missing named authorization prerequisite", err)
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

func TestT171SmokeRefusesMissingBehavioralEvidence(t *testing.T) {
	for _, alias := range []string{"", "claude", "codex", "grok"} {
		if err := liveSmokePrerequisite(alias); err == nil {
			t.Fatalf("alias %q substituted for literal subscription ID", alias)
		}
	}
	for _, provider := range PlanIDs {
		err := liveSmokePrerequisite(provider)
		for _, missing := range []error{errLiveSteerEvidence, errLiveAbortEvidence, errLiveHostToolsEvidence} {
			if !errors.Is(err, missing) {
				t.Fatalf("smoke omitted blocker %v: %v", missing, err)
			}
		}
	}
}
