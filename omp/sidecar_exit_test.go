// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import "testing"

func TestReleaseStartedSidecarSkipsHookWhenItNeverServed(t *testing.T) {
	called := false
	SetServingExitHook(func(int) { called = true })
	t.Cleanup(func() { SetServingExitHook(nil) })
	releaseStartedSidecar(424242, nil, nil)
	if called {
		t.Fatal("exit hook ran for a sidecar that never served the socket")
	}
}

func TestReleaseStartedSidecarNotifiesWhenItServed(t *testing.T) {
	var got int
	SetServingExitHook(func(pid int) { got = pid })
	t.Cleanup(func() { SetServingExitHook(nil) })
	ensureMu.Lock()
	serving[31337] = true
	ensureMu.Unlock()
	releaseStartedSidecar(31337, nil, nil)
	if got != 31337 {
		t.Fatalf("exit hook pid = %d, want 31337", got)
	}
}
