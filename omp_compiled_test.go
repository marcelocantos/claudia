// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompiledAuthWithoutBun(t *testing.T) {
	bin := os.Getenv("CLAUDIA_TEST_COMPILED_DIR")
	if bin == "" {
		t.Skip("run make test-compiled-sidecar to build the native helpers")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")
	helper := filepath.Join(bin, "claudia-omp-auth")
	// Empty credentials plus --no-login cannot contact a provider or open
	// a browser. Reaching this refusal proves imports/native addon startup.
	cmd := exec.CommandContext(t.Context(), helper, "refresh", "anthropic", "--no-login")
	cmd.Stdin = strings.NewReader("{}")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "noninteractive credential refresh requires an existing refresh token") {
		t.Fatalf("compiled auth refusal = %q, %v", out, err)
	}
	_, err := execBunLogin(t.Context(), "bun", helper, "refresh", "anthropic", "--no-login", "{}")
	if err == nil || !strings.Contains(err.Error(), "noninteractive refresh helper failed: exit status 1") {
		t.Fatalf("auth runner did not execute the compiled helper: %v", err)
	}
	_, err = execBunLogin(t.Context(), "bun", helper, "login", "anthropic")
	if err == nil || !strings.Contains(err.Error(), "never starts an interactive plan login") {
		t.Fatalf("compiled auth bypassed test login guard: %v", err)
	}
}
