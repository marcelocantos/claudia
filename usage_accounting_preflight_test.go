// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

func TestUsageAccountingActivationRefusesImplicitRuntime(t *testing.T) {
	root := "/private/tmp/claudia-usage-live.fixture"
	a := usageLiveActivation{
		Root: root, SourceDir: "/private/tmp/claudia-source", BrokerBinary: "/private/tmp/claudia-source/bin/claudia",
		Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), BrokerSHA256: strings.Repeat("c", 64),
		BrokerPID: 100, SidecarPID: 101, BrokerStart: "broker-start", SidecarStart: "sidecar-start", NoRefresh: true,
		Models: map[string]string{},
	}
	for _, provider := range usageLiveProviders {
		a.Models[provider] = "explicit-model"
	}
	env := map[string]string{
		"CLAUDIA_USAGE_ROOT": root, "CLAUDIA_BROKER_SOCKET": filepath.Join(root, "broker.sock"), omp.SocketEnv: filepath.Join(root, "omp.sock"),
		"XDG_STATE_HOME": filepath.Join(root, "state"), "CLAUDIA_OMP_SPOOL_DIR": filepath.Join(root, "spool"),
		"CLAUDIA_OMP_SERVER": filepath.Join(a.SourceDir, "sidecar", "server.ts"), "CLAUDIA_NO_BROKER": "0", omp.NoNetworkEnv: "1",
	}
	getenv := func(k string) string { return env[k] }
	if err := a.validate(getenv); err != nil {
		t.Fatal(err)
	}
	for key, original := range env {
		t.Run(key, func(t *testing.T) {
			delete(env, key)
			if err := a.validate(getenv); err == nil {
				t.Fatal("missing isolate setting accepted")
			}
			env[key] = "/Users/owner/.local/state/claudia/omp.sock"
			if err := a.validate(getenv); err == nil {
				t.Fatal("foreign runtime accepted")
			}
			env[key] = original
		})
	}
	for _, mutate := range []func(*usageLiveActivation){
		func(a *usageLiveActivation) { a.NoRefresh = false }, func(a *usageLiveActivation) { a.Commit = "" },
		func(a *usageLiveActivation) { a.BrokerSHA256 = "" }, func(a *usageLiveActivation) { a.SidecarPID = a.BrokerPID },
		func(a *usageLiveActivation) { a.Root = "/Users/owner/.local/state/claudia" },
		func(a *usageLiveActivation) { a.Models = nil },
	} {
		invalid := a
		mutate(&invalid)
		if err := invalid.validate(getenv); err == nil {
			t.Fatal("incomplete activation accepted")
		}
	}
}
