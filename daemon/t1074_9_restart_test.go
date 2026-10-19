// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
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

// TestT10749BrokerOwnsItsControlFile drives daemon.New with no control file
// on disk. The broker creates its own, is its only owner while it runs (a
// second owner is refused), and answers the consumer's sqlpipe link on the
// control socket beside its broker socket. Close releases both.
func TestT10749BrokerOwnsItsControlFile(t *testing.T) {
	if os.Getenv("T1074_9_CHILD") == "1" {
		t.Skip("restart child")
	}
	dir := t.TempDir()
	sock := filepath.Join("/tmp", fmt.Sprintf("t10749-own-%d.sock", os.Getpid()))
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
	path := filepath.Join(dir, controlplane.ClaudiaDBFile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("broker did not create its control file: %v", err)
	}
	if second, err := claudia.OpenControlPublisher(path, sock+".other"); !errors.Is(err, controlplane.ErrOwnerHeld) {
		if second != nil {
			_ = second()
		}
		t.Fatalf("second owner = %v, want ErrOwnerHeld", err)
	}
	replica, err := controlplane.OpenReplica(filepath.Join(t.TempDir(), controlplane.JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.Write(func(db *sqlpipe.Database) error {
		return controlplane.PublishDesiredSeat(db, controlplane.DesiredSeat{Seat: "seat-l", Generation: 1, Revision: 1, Provider: "grok", Model: "m", WorkDir: dir, ConfigJSON: `{}`})
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	linked := make(chan error, 1)
	go func() { linked <- replica.Link(ctx, claudia.ControlSocketPath(sock)) }()
	claudia.RecordObservedReceipt(&claudia.RequestReceipt{
		RequestID: "over-link", SessionID: "generation", Seat: "seat",
		State: claudia.ReceiptAccepted, AttemptID: "attempt", TurnIDs: []string{},
		AcceptedAt: "2026-10-19T01:00:00.000000000Z", UpdatedAt: "2026-10-19T01:00:00.000000000Z",
	})
	waitFor(t, "the broker's receipt in the consumer's own file", func() bool {
		var n int64
		_ = replica.Read(func(db *sqlpipe.Database) error {
			qr, err := db.Query(`SELECT COUNT(*) FROM claudia_request_receipt WHERE request_id = 'over-link'`)
			if err == nil {
				n = qr.Rows[0][0].(int64)
			}
			return err
		})
		return n == 1
	})
	cancel()
	if err := <-linked; err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claudia.ControlSocketPath(sock)); !os.IsNotExist(err) {
		t.Fatalf("control socket left behind: %v", err)
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
