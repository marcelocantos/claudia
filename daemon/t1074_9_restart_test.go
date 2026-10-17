// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/controlplane"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// TestT10749DaemonRestartKeepsSession re-execs this test binary twice.
// Each child is a new OS process that calls daemon.New. The second process
// is a bounce: RequireResume stays set, and the session id does not change.
// The child process exiting is the event this waits on. go test -timeout
// is the clock.
func TestT10749DaemonRestartKeepsSession(t *testing.T) {
	if os.Getenv("T1074_9_CHILD") == "1" {
		runT10749Child(t)
		return
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sqlpipe.OpenDatabase(filepath.Join(dir, controlplane.ClaudiaDBFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := controlplane.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := controlplane.PublishDesiredSeat(db, controlplane.DesiredSeat{
		Seat: "seat-a", Generation: 1, Revision: 1, Provider: "grok", Model: "m",
		WorkDir: filepath.Join(dir, "work"), ConfigJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := controlplane.PublishGrant(db, controlplane.Grant{
		Seat: "seat-a", GrantID: "g", Revision: 1, Scope: `{"name":"read"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestT10749DaemonRestartKeepsSession$", "-test.count=1")
		cmd.Env = append(os.Environ(), "T1074_9_CHILD=1", "T1074_9_DIR="+dir, "T1074_9_SEAT=seat-a")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, out)
		}
	}

	body, err := os.ReadFile(filepath.Join(dir, "sessions.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.FieldsFunc(string(body), func(r rune) bool { return r == '\n' })
	if len(lines) != 2 {
		t.Fatalf("session lines = %q", lines)
	}
	first := strings.Fields(lines[0])
	second := strings.Fields(lines[1])
	if len(first) != 2 || len(second) != 2 || first[0] == "" || first[0] != second[0] {
		t.Fatalf("sessions = %q", lines)
	}
	if first[1] != "false" || second[1] != "true" {
		t.Fatalf("resume flags = %q", lines)
	}

	db, err = sqlpipe.OpenDatabase(filepath.Join(dir, controlplane.ClaudiaDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statuses, err := controlplane.SeatStatuses(db)
	if err != nil {
		t.Fatal(err)
	}
	status := statuses["seat-a"]
	if status.State != "running" || status.SessionID != first[0] || status.RuntimeID != first[0] {
		t.Fatalf("status = %+v", status)
	}
}

func TestT10749MissingControlFileStillServes(t *testing.T) {
	if os.Getenv("T1074_9_CHILD") == "1" {
		t.Skip("restart child")
	}
	dir := t.TempDir()
	sock := filepath.Join("/tmp", fmt.Sprintf("t10749-miss-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })
	d, err := New(Options{
		SocketPath:     sock,
		StateDir:       dir,
		UsageFetch:     func(context.Context) ([]claudia.PlanUsage, error) { return nil, nil },
		DisableResume:  true,
		DisableIntel:   true,
		DisableMCPHost: true,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, controlplane.ClaudiaDBFile)); !os.IsNotExist(err) {
		t.Fatalf("missing control file was created: %v", err)
	}
}

func runT10749Child(t *testing.T) {
	dir := os.Getenv("T1074_9_DIR")
	seat := os.Getenv("T1074_9_SEAT")
	if dir == "" || seat == "" {
		t.Fatal("child dir")
	}
	logPath := filepath.Join(dir, "sessions.txt")
	sock := filepath.Join("/tmp", fmt.Sprintf("t10749-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })
	d, err := New(Options{
		SocketPath:     sock,
		StateDir:       dir,
		UsageFetch:     func(context.Context) ([]claudia.PlanUsage, error) { return nil, nil },
		DisableResume:  true,
		DisableIntel:   true,
		DisableMCPHost: true,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		launchers: &claudia.RegistryLaunchers{Start: func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return nil, err
			}
			_, werr := fmt.Fprintf(f, "%s %v\n", cfg.SessionID, cfg.RequireResume)
			cerr := f.Close()
			if werr != nil {
				return nil, werr
			}
			if cerr != nil {
				return nil, cerr
			}
			// Do not clear RequireResume. StartStub returns cfg.SessionID.
			return claudia.StartStub(ctx, cfg, nil)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	def := d.reg.Def(seat)
	if def == nil || def.SessionID == "" {
		t.Fatalf("def = %+v", def)
	}
}
