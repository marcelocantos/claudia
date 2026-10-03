// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This is an operator activation receipt, not runtime attestation. The operator
// must bind the loaded immutable sidecar tree to the serving process; hashing a
// mutable server.ts after startup cannot provide that evidence (see docs).
type liveActivation struct {
	BrokerBinary           string `json:"broker_binary"`
	BrokerSHA256           string `json:"broker_sha256"`
	BrokerSocket           string `json:"broker_socket"`
	BrokerPID              int    `json:"broker_pid"`
	BrokerStart            string `json:"broker_start"`
	SidecarSocket          string `json:"sidecar_socket"`
	SidecarPID             int    `json:"sidecar_pid"`
	SidecarStart           string `json:"sidecar_start"`
	LoadedArtifactEvidence string `json:"loaded_artifact_evidence"`
}

type liveHarness struct{ liveActivation }

func explicitLiveBinary(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("set JEVONS_BROKER_BIN to an absolute installed claudia executable; no discovery fallback")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("broker must be a regular executable")
	}
	return real, nil
}

func (a liveActivation) validate(bin, brokerSocket, sidecarSocket string) error {
	if a.BrokerBinary != bin || !filepath.IsAbs(brokerSocket) || !filepath.IsAbs(sidecarSocket) ||
		a.BrokerSocket != brokerSocket || a.SidecarSocket != sidecarSocket || brokerSocket == sidecarSocket {
		return fmt.Errorf("activation receipt must match explicit binary and distinct absolute CLAUDIA_BROKER_SOCKET/CLAUDIA_OMP_SOCKET")
	}
	if a.BrokerPID <= 1 || a.SidecarPID <= 1 || a.BrokerPID == a.SidecarPID ||
		strings.TrimSpace(a.BrokerStart) == "" || strings.TrimSpace(a.SidecarStart) == "" ||
		strings.TrimSpace(a.LoadedArtifactEvidence) == "" {
		return fmt.Errorf("activation receipt needs distinct serving PIDs, start times and loaded-artifact evidence")
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != a.BrokerSHA256 {
		return fmt.Errorf("broker artifact hash differs from activation receipt")
	}
	return nil
}

func newLiveHarness(t *testing.T) liveHarness {
	t.Helper()
	bin := liveBrokerBin(t)
	receipt := os.Getenv("CLAUDIA_OMP_ACTIVATION_RECEIPT")
	if !filepath.IsAbs(receipt) {
		t.Fatal("set CLAUDIA_OMP_ACTIVATION_RECEIPT to an absolute operator activation receipt")
	}
	raw, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var h liveHarness
	if err := json.Unmarshal(raw, &h.liveActivation); err != nil {
		t.Fatal(err)
	}
	if err := h.validate(bin, os.Getenv("CLAUDIA_BROKER_SOCKET"), os.Getenv(SocketEnv)); err != nil {
		t.Fatal(err)
	}
	h.preflight(t)
	return h
}

func runLiveCommand(ctx context.Context, bin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = time.Second
	// stdout is decoded by callers; stderr may contain provider details and is
	// deliberately not echoed into test logs. argv never contains credentials.
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", filepath.Base(bin), err)
	}
	return out, nil
}

func (h liveHarness) command(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	out, err := runLiveCommand(ctx, h.BrokerBinary, args...)
	if err != nil {
		t.Fatalf("broker %s: %v", args[1], err)
	}
	return out
}

func (h liveHarness) preflight(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, p := range []struct {
		pid           int
		start, socket string
	}{
		{h.BrokerPID, h.BrokerStart, h.BrokerSocket}, {h.SidecarPID, h.SidecarStart, h.SidecarSocket},
	} {
		out, err := runLiveCommand(ctx, "/bin/ps", "-p", strconv.Itoa(p.pid), "-o", "lstart=")
		if err != nil || strings.TrimSpace(string(out)) != p.start {
			t.Fatal("serving process start time differs from activation receipt")
		}
		out, err = runLiveCommand(ctx, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(p.pid), "-U", "-Fn")
		if err != nil || !hasLiveSocket(out, p.socket) {
			t.Fatal("receipt PID does not own selected Unix socket")
		}
		if !Listening(ctx, p.socket) {
			t.Fatal("selected socket is not listening; harness never calls Ensure")
		}
	}
	out, err := runLiveCommand(ctx, "/bin/ps", "-p", strconv.Itoa(h.BrokerPID), "-o", "comm=")
	if err != nil || strings.TrimSpace(string(out)) != h.BrokerBinary {
		t.Fatal("serving broker executable differs from selected binary")
	}
	raw, err := os.ReadFile(pidPath(h.SidecarSocket))
	if err != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(h.SidecarPID) {
		t.Fatal("sidecar pid file differs from serving receipt")
	}
	t.Logf("activation preflight: broker PID %d, sidecar PID %d; operator loaded-artifact evidence: %s", h.BrokerPID, h.SidecarPID, h.LoadedArtifactEvidence)
}

func hasLiveSocket(out []byte, socket string) bool {
	for _, line := range strings.Split(string(out), "\n") {
		if line == "n"+socket {
			return true
		}
	}
	return false
}

type liveCommand func(context.Context, ...string) ([]byte, error)

func recoverLivePlan(ctx context.Context, run liveCommand, plan string) (before, after string, err error) {
	if !known(plan) {
		return "", "", fmt.Errorf("unknown subscription plan")
	}
	health := func() (string, error) {
		out, err := run(ctx, "broker", "auth-status", "--json")
		if err != nil {
			return "", err
		}
		var status struct {
			Plans []PlanHealth `json:"plans"`
		}
		if err := json.Unmarshal(out, &status); err != nil {
			return "", fmt.Errorf("invalid auth-status JSON")
		}
		for _, p := range status.Plans {
			if p.Provider == plan {
				return p.State, nil
			}
		}
		return "", fmt.Errorf("auth-status omitted selected plan")
	}
	before, err = health()
	if err != nil {
		return
	}
	if _, err = run(ctx, "broker", "auth-recover", "--no-login", plan); err != nil {
		return
	}
	after, err = health()
	if err == nil {
		if after != HealthOK {
			err = fmt.Errorf("no-login recovery left plan %s", after)
		} else {
			err = errLiveRenewalEvidence
		}
	}
	return
}

func bounceCommands(getenv func(string) string) (restart, ready []string, err error) {
	if getenv("CLAUDIA_OMP_BOUNCE_AUTHORIZED") != "restart-jevonsd" {
		return nil, nil, errLiveBouncePermit
	}
	for name, dst := range map[string]*[]string{"CLAUDIA_OMP_BOUNCE_ARGV": &restart, "CLAUDIA_OMP_READY_ARGV": &ready} {
		if err := json.Unmarshal([]byte(getenv(name)), dst); err != nil || len(*dst) == 0 || !filepath.IsAbs((*dst)[0]) {
			return nil, nil, fmt.Errorf("%s must be JSON argv with absolute executable", name)
		}
	}
	return restart, ready, nil
}

func waitLiveReady(ctx context.Context, ready func(context.Context) bool) error {
	for {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		ok := ready(probe)
		cancel()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// The CLI accepts literal subscription IDs, but the correlated
// steer/abort/tool oracles remain held pending the event slice. Keep enabled smoke red before any command, rather than award
// partial coverage for protocol acknowledgements.
var (
	errLiveRenewalEvidence   = errors.New("OMP_RENEWAL_EVIDENCE_UNAVAILABLE: auth recovery exposes no refreshed/no-op outcome; health is not renewal proof")
	errLiveBouncePermit      = errors.New("OMP_BOUNCE_AUTHORIZATION_REQUIRED: coordinated CLAUDIA_OMP_BOUNCE_AUTHORIZED=restart-jevonsd prerequisite missing")
	errLiveSteerEvidence     = errors.New("OMP_STEER_EVIDENCE_UNAVAILABLE: correlated model uptake of a steer is not observable through this harness")
	errLiveAbortEvidence     = errors.New("OMP_ABORT_EVIDENCE_UNAVAILABLE: interrupt acknowledgement does not prove correlated terminal abort")
	errLiveHostToolsEvidence = errors.New("OMP_HOST_TOOLS_EVIDENCE_UNAVAILABLE: standalone CLI does not attest intended jevons host-tool arming")
)

func liveSmokePrerequisite(provider string) error {
	if !known(provider) {
		return fmt.Errorf("OMP_LITERAL_PROVIDER_REQUIRED: choose anthropic, openai-codex, xai-oauth or cursor")
	}
	return errors.Join(errLiveSteerEvidence, errLiveAbortEvidence, errLiveHostToolsEvidence)
}
