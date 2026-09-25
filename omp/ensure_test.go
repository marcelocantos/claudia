// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestT865LiveSidecarAcceptsLaunchSteerAbort(t *testing.T) {
	testEnsureSidecarVerbs(t)
}

func TestEnsureStartsDetachedSidecar(t *testing.T) {
	testEnsureSidecarVerbs(t)
}

func testEnsureSidecarVerbs(t *testing.T) {
	if _, err := os.Stat(ServerScript()); err != nil {
		t.Skip("sidecar/server.ts missing")
	}
	dir, err := os.MkdirTemp("/tmp", "omp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "omp.sock")
	t.Setenv(SocketEnv, socket)
	t.Setenv("JEVONS_SPOOL_DIR", filepath.Join(dir, "spool"))
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	path, err := Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = StopSidecar(socket) })
	if path != socket {
		t.Fatalf("socket = %s, want %s", path, socket)
	}
	if !Listening(ctx, socket) {
		t.Fatal("sidecar is not listening")
	}
	again, err := Ensure(ctx)
	if err != nil || again != socket {
		t.Fatalf("second Ensure = %s %v", again, err)
	}
	conn, err := Dial(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Send(Message{Op: OpLoad, Seat: "smoke", Provider: XAIOAuth, Model: "grok-4.6", Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	ev, err := conn.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != "ready" {
		t.Fatalf("load = %+v, want ready", ev)
	}
	if err := conn.Send(Message{Op: OpPrompt, Seat: "smoke", Text: "ping"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(Message{Op: OpSteer, Seat: "smoke", Text: "nudge"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(Message{Op: OpAbort, Seat: "smoke"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(Message{Op: OpTool, CallID: "x", Result: "jevons_ok"}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSurvivesParentExit(t *testing.T) {
	if _, err := os.Stat(ServerScript()); err != nil {
		t.Skip("sidecar/server.ts missing")
	}
	dir, err := os.MkdirTemp("/tmp", "omp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "child.sock")
	t.Setenv(SocketEnv, socket)
	t.Setenv("JEVONS_SPOOL_DIR", filepath.Join(dir, "spool"))
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if _, err := Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = StopSidecar(socket) })
	time.Sleep(100 * time.Millisecond)
	if !Listening(ctx, socket) {
		t.Fatal("sidecar died when the ensurer returned")
	}
}

func TestSpoolAppendsDatedLog(t *testing.T) {
	dir := t.TempDir()
	now := "2026-09-25T15:04:05.000Z"
	rec := `{"ts":"2026-09-25T15:04:05.000Z","seat":"jevons-po","type":"text","text":"hi"}`
	out, err := runSpool(t, dir, rec, now)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "events-2026-09-25.log")
	if strings.TrimSpace(out) != want {
		t.Fatalf("path = %q, want %q", strings.TrimSpace(out), want)
	}
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"seat":"jevons-po"`) {
		t.Fatalf("record missing seat: %s", body)
	}
	if !strings.Contains(string(body), `"ts":"2026-09-25T15:04:05.000Z"`) {
		t.Fatalf("record missing ts: %s", body)
	}
}

func TestSpoolLateEventKeepsTimestampOnLiveDay(t *testing.T) {
	dir := t.TempDir()
	if _, err := runSpool(t, dir,
		`{"ts":"2026-09-25T01:00:00.000Z","seat":"a","type":"ready"}`,
		"2026-09-25T01:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := runSpool(t, dir,
		`{"ts":"2026-09-26T00:00:01.000Z","seat":"a","type":"text","text":"next"}`,
		"2026-09-26T00:00:01.000Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := runSpool(t, dir,
		`{"ts":"2026-09-25T23:59:59.000Z","seat":"a","type":"text","text":"late"}`,
		"2026-09-26T00:01:00.000Z"); err != nil {
		t.Fatal(err)
	}
	closed, err := os.ReadFile(filepath.Join(dir, "events-2026-09-25.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(closed), "late") {
		t.Fatal("closed day was reopened")
	}
	live, err := os.ReadFile(filepath.Join(dir, "events-2026-09-26.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), `"ts":"2026-09-25T23:59:59.000Z"`) {
		t.Fatalf("late event lost its timestamp: %s", live)
	}
	if !strings.Contains(string(live), "late") {
		t.Fatalf("late event missing from live day: %s", live)
	}
}

func TestSpoolDoesNotCompress(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(filepath.Dir(ServerScript()), "spool.ts"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, ban := range []string{"hfsCompression", "ditto", "applesauce"} {
		if strings.Contains(body, ban) {
			t.Fatalf("shim must not compress (%s)", ban)
		}
	}
}

func TestSpoolAppendDoesNotCompressFile(t *testing.T) {
	dir := t.TempDir()
	now := "2026-09-25T15:04:05.000Z"
	rec := `{"ts":"2026-09-25T15:04:05.000Z","seat":"jevons-po","type":"text","text":"hi"}`
	if _, err := runSpool(t, dir, rec, now); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events-2026-09-25.log")
	if hfsCompressed(t, path) {
		t.Fatal("shim append set UF_COMPRESSED")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"seat":"jevons-po"`) {
		t.Fatalf("read after append lost the record: %s", body)
	}
}

func runSpool(t *testing.T, dir, rec, now string) (string, error) {
	t.Helper()
	script := filepath.Join(filepath.Dir(ServerScript()), "spool.ts")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", script, "append", dir, rec, now)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func hfsCompressed(t *testing.T, path string) bool {
	t.Helper()
	out, err := exec.Command("stat", "-f", "%f", path).Output()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	var flags uint64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &flags); err != nil {
		t.Fatalf("stat flags %q: %v", out, err)
	}
	const ufCompressed = 0x20
	return flags&ufCompressed != 0
}
