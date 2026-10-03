// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A newly compiled process reads the installed item directly through Security,
// bypassing Store.SealPath, security(1), CLI dispatch, and daemon RPC. No fixture
// or enrollment is created. Run only after the parent approves this live probe.
func TestT170LiveDirectKeychainACL(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	probe := t170BuildACLProbe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, probe, KeychainService, keychainAccount)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("native probe did not complete: %v (no denial evidence)", err)
	}
	if err := t170ACLRefusal(out); err != nil {
		t.Fatal(err)
	}
	t.Log("direct Security content read refused from an untrusted process; item found in an unlocked Keychain; interaction disabled")
}

func t170BuildACLProbe(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Security.framework requires macOS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	probe := filepath.Join(t.TempDir(), "untrusted-acl-probe")
	cmd := exec.CommandContext(ctx, "/usr/bin/clang", "-Werror", "-Wno-deprecated-declarations", "-framework", "Security", "-framework", "CoreFoundation", "testdata/t170/acl_probe.c", "-o", probe)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile native probe: %v: %s", err, out)
	}
	return probe
}

// Only the content read may provide denial evidence. In particular, exit 44
// (often item-not-found), missing/locked items and process failures never pass.
func t170ACLRefusal(out []byte) error {
	fields := strings.Fields(string(out))
	if len(fields) == 2 && fields[0] == "read" && (fields[1] == "-25293" || fields[1] == "-25308") {
		return nil // errSecAuthFailed or errSecInteractionNotAllowed
	}
	// Do not echo even malformed helper output: it could contain secret bytes.
	return fmt.Errorf("no direct OS ACL denial evidence (expected read-stage errSecAuthFailed or errSecInteractionNotAllowed)")
}

func TestT170ACLProbeCompilesWithoutExecuting(t *testing.T) {
	t170BuildACLProbe(t)
}

func TestT170ACLRefusalControls(t *testing.T) {
	for _, result := range []string{"read -25293\n", "read -25308\n"} {
		if err := t170ACLRefusal([]byte(result)); err != nil {
			t.Fatal(err)
		}
	}
	for _, result := range []string{"read 0", "read -25300", "read 44", "lookup -25308", "locked 0", "interaction -25293", "read -50", "", "errSecAuthFailed", "read -25308 extra"} {
		if err := t170ACLRefusal([]byte(result)); err == nil {
			t.Fatalf("accepted non-evidence %q", result)
		}
	}
}
