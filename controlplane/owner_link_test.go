// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/internal/wallclockguard"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// asBroker makes this test process the broker daemon for the test's length.
func asBroker(t *testing.T) {
	t.Helper()
	broker.MarkSelfHosted()
	t.Cleanup(broker.AllowInProcessClientsForTest)
}

// brokerDirs is a broker's state directory and broker socket path. The
// socket stays short, under /tmp: a unix socket path is limited to ~104
// bytes, and t.TempDir() on macOS is longer.
func brokerDirs(t *testing.T) (dbPath, controlSocket string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, ClaudiaDBFile), claudia.ControlSocketPath(filepath.Join(dir, "broker.sock"))
}

func publishOwned(t *testing.T, dbPath, sock string) func() error {
	t.Helper()
	closeFn, err := PublishFile(dbPath, sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeFn() })
	return closeFn
}

func ownerRead(t *testing.T, fn func(*sqlpipe.Database) error) {
	t.Helper()
	ownedMu.Lock()
	p := owned
	ownedMu.Unlock()
	if p == nil {
		t.Fatal("no owner plane")
	}
	if err := p.withDB(fn); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	backstop := wallclockguard.UntilTestTimeout(t)
	for backstop.Err() == nil {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func count(t *testing.T, db *sqlpipe.Database, table string) int64 {
	t.Helper()
	qr, err := db.Query("SELECT COUNT(*) FROM " + table)
	if err != nil {
		t.Fatal(err)
	}
	return qr.Rows[0][0].(int64)
}

// A library consumer is not the broker, so it cannot open, create, or bind
// the broker's file. This is what jevonsd did (internal/cost/replica.go,
// OpenReplica: controlplane.Open + controlplane.Bind).
func TestControlFileRefusesANonBrokerProcess(t *testing.T) {
	broker.AllowInProcessClientsForTest()
	dbPath, sock := brokerDirs(t)
	if closeFn, err := PublishFile(dbPath, sock); !errors.Is(err, ErrNotOwner) {
		if closeFn != nil {
			_ = closeFn()
		}
		t.Fatalf("consumer open = %v, want ErrNotOwner", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("refused open still touched the file: %v", err)
	}
	if _, err := OpenReplica(dbPath); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("replica of the broker's file = %v, want ErrNotOwner", err)
	}
	if err := ReconcileFile(context.Background(), dbPath, nil); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("reconcile with no owner plane = %v, want ErrNotOwner", err)
	}
}

// A second owner, in this process or another, is refused loudly, naming the
// holder, instead of racing the first for SQLite's write lock.
func TestControlFileRefusesASecondOwner(t *testing.T) {
	if os.Getenv("CP_SECOND_OWNER_CHILD") != "" {
		broker.MarkSelfHosted()
		_, err := PublishFile(os.Getenv("CP_SECOND_OWNER_CHILD"), os.Getenv("CP_SECOND_OWNER_SOCK")+".child")
		fmt.Printf("CHILD-RESULT %v\n", err)
		return
	}
	asBroker(t)
	dbPath, sock := brokerDirs(t)
	closeFn := publishOwned(t, dbPath, sock)
	if second, err := PublishFile(dbPath, sock+".2"); !errors.Is(err, ErrOwnerHeld) {
		if second != nil {
			_ = second()
		}
		t.Fatalf("second owner in this process = %v, want ErrOwnerHeld", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestControlFileRefusesASecondOwner$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CP_SECOND_OWNER_CHILD="+dbPath, "CP_SECOND_OWNER_SOCK="+sock)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	want := fmt.Sprintf("held by pid %d", os.Getpid())
	if !strings.Contains(string(out), "CHILD-RESULT "+ErrOwnerHeld.Error()) || !strings.Contains(string(out), want) {
		t.Fatalf("second owner in another process was not refused naming pid %d:\n%s", os.Getpid(), out)
	}
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	again, err := PublishFile(dbPath, sock)
	if err != nil {
		t.Fatalf("reopen after the owner closed: %v", err)
	}
	_ = again()
}

// The two processes' files converge over the socket, both ways, and a
// reconnect's handshake delivers what either side committed while the link
// was down. Neither side opens the other's file.
func TestReplicaLinkDeliversBothWaysAndAcrossReconnect(t *testing.T) {
	asBroker(t)
	dbPath, sock := brokerDirs(t)
	publishOwned(t, dbPath, sock)
	replica, err := OpenReplica(filepath.Join(t.TempDir(), JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.Write(func(db *sqlpipe.Database) error {
		if err := PublishDesiredSeat(db, DesiredSeat{Seat: "seat-a", Generation: 1, Revision: 1, Provider: "grok", Model: "m", WorkDir: "/tmp/w", ConfigJSON: `{"model":"m"}`}); err != nil {
			return err
		}
		return PublishGrant(db, Grant{Seat: "seat-a", GrantID: "g", Revision: 1, Scope: `{"name":"read"}`})
	}); err != nil {
		t.Fatal(err)
	}
	type liveLink struct {
		cancel context.CancelFunc
		done   chan struct{}
		err    error
	}
	start := func() *liveLink {
		ctx, cancel := context.WithCancel(t.Context())
		l := &liveLink{cancel: cancel, done: make(chan struct{})}
		go func() {
			l.err = replica.Link(ctx, sock)
			close(l.done)
		}()
		return l
	}
	stop := func(l *liveLink) {
		l.cancel()
		<-l.done
		if l.err != nil {
			t.Errorf("link: %v", l.err)
		}
	}
	// A wait that only polls the destination table misses a refused
	// reconnect: Link returns nil on the closed conn (EOF), and the
	// test sits until the package timeout (🎯T227).
	requireUp := func(l *liveLink, what string) {
		t.Helper()
		select {
		case <-l.done:
			t.Fatalf("link ended while waiting for %s: %v", what, l.err)
		default:
		}
	}
	live := start()
	eventually(t, "desired seat at the broker", func() bool {
		requireUp(live, "desired seat at the broker")
		var n int
		ownerRead(t, func(db *sqlpipe.Database) error {
			got, err := DesiredSeats(db)
			n = len(got)
			if n == 1 && (got[0].Seat != "seat-a" || got[0].ConfigJSON != `{"model":"m"}`) {
				t.Fatalf("delivered desired = %+v", got)
			}
			return err
		})
		return n == 1
	})
	ownerRead(t, func(db *sqlpipe.Database) error {
		return PublishSeatStatus(db, SeatStatus{Seat: "seat-a", SessionID: "sess", Generation: 1, Revision: 1, RuntimeID: "sess", State: statusRunning, StatusJSON: `{}`})
	})
	eventually(t, "seat status at the consumer", func() bool {
		requireUp(live, "seat status at the consumer")
		var got map[string]SeatStatus
		_ = replica.Read(func(db *sqlpipe.Database) (err error) { got, err = SeatStatuses(db); return err })
		return got["seat-a"].SessionID == "sess"
	})
	stop(live)

	// Both sides commit while no link is up.
	claudia.RecordObservedReceipt(&claudia.RequestReceipt{RequestID: "offline", SessionID: "s", Seat: "seat-a",
		State: claudia.ReceiptAccepted, AttemptID: "a", TurnIDs: []string{}, AcceptedAt: "2026-10-19T00:00:00Z", UpdatedAt: "2026-10-19T00:00:00Z"})
	if err := replica.Write(func(db *sqlpipe.Database) error {
		return PublishControlIntent(db, ControlIntent{OperationID: "op-offline", Seat: "seat-a", Generation: 1, Kind: "interrupt", PayloadJSON: `{}`})
	}); err != nil {
		t.Fatal(err)
	}
	live = start()
	defer stop(live)
	eventually(t, "offline intent at the broker", func() bool {
		requireUp(live, "offline intent at the broker")
		var n int
		ownerRead(t, func(db *sqlpipe.Database) error {
			got, err := ControlIntents(db)
			n = len(got)
			return err
		})
		return n == 1
	})
	eventually(t, "offline receipt at the consumer", func() bool {
		requireUp(live, "offline receipt at the consumer")
		var n int64
		_ = replica.Read(func(db *sqlpipe.Database) error { n = count(t, db, ReceiptTable); return nil })
		return n == 1
	})
}

// Under host load the reconnect in TestReplicaLinkDeliversBothWaysAndAcrossReconnect
// used to miss the offline intent: the broker refused the same-pid redial
// while the previous link was still ending, Link returned, and eventually
// never re-checked that the link was up. Spin the scheduler and redial
// repeatedly so a refused handshake fails here instead of at make gate.
func TestReplicaLinkReconnectUnderSchedLoad(t *testing.T) {
	asBroker(t)
	var stopLoad atomic.Bool
	var load sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	if workers < 4 {
		workers = 4
	}
	load.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer load.Done()
			n := 0
			for !stopLoad.Load() {
				n++
			}
			_ = n
		}()
	}
	t.Cleanup(func() { stopLoad.Store(true); load.Wait() })

	dbPath, sock := brokerDirs(t)
	publishOwned(t, dbPath, sock)
	replica, err := OpenReplica(filepath.Join(t.TempDir(), JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()

	start := func() (cancel context.CancelFunc, done chan struct{}, errp *error) {
		ctx, cancel := context.WithCancel(t.Context())
		done = make(chan struct{})
		var linkErr error
		errp = &linkErr
		go func() {
			linkErr = replica.Link(ctx, sock)
			close(done)
		}()
		return cancel, done, errp
	}
	requireUp := func(done chan struct{}, errp *error, what string) {
		t.Helper()
		select {
		case <-done:
			t.Fatalf("link ended while waiting for %s: %v", what, *errp)
		default:
		}
	}

	const cycles = 20
	for i := 0; i < cycles; i++ {
		op := fmt.Sprintf("op-load-%d", i)
		if err := replica.Write(func(db *sqlpipe.Database) error {
			return PublishControlIntent(db, ControlIntent{OperationID: op, Seat: "seat-a", Generation: 1, Kind: "interrupt", PayloadJSON: `{}`})
		}); err != nil {
			t.Fatal(err)
		}
		cancel, done, errp := start()
		eventually(t, op+" at the broker", func() bool {
			requireUp(done, errp, op+" at the broker")
			var found bool
			ownerRead(t, func(db *sqlpipe.Database) error {
				got, err := ControlIntents(db)
				for _, in := range got {
					if in.OperationID == op {
						found = true
						break
					}
				}
				return err
			})
			return found
		})
		cancel()
		<-done
		if *errp != nil {
			t.Fatalf("link %d: %v", i, *errp)
		}
	}
}

type warnings struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnings) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		return nil
	}
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool { fmt.Fprintf(&b, " %s=%v", a.Key, a.Value); return true })
	w.mu.Lock()
	w.msgs = append(w.msgs, b.String())
	w.mu.Unlock()
	return nil
}
func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnings) WithGroup(string) slog.Handler      { return w }

// The 2026-10-19 isolate run logged "database is locked" on seat-pump receipt
// and usage writes: jevonsd had opened the broker's file as a second writer
// and ran sqlpipe between the two files in its own process. Here the seat
// pump publishes from several goroutines while the consumer links, drops
// and relinks (each a fresh handshake), writes its own tables, and a plain
// reader holds read locks on the broker's file. Nothing may be refused, and
// every fact must arrive on both sides.
func TestConcurrentSeatPumpWritesAndHandshakesLoseNothing(t *testing.T) {
	asBroker(t)
	w := &warnings{}
	prev := slog.Default()
	slog.SetDefault(slog.New(w))
	t.Cleanup(func() { slog.SetDefault(prev) })
	dbPath, sock := brokerDirs(t)
	publishOwned(t, dbPath, sock)
	replica, err := OpenReplica(filepath.Join(t.TempDir(), JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()

	const pumps, each, seats = 4, 100, 20
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var bg sync.WaitGroup
	// The consumer: link, write, drop, relink, for as long as the pumps run.
	var links atomic.Int32
	bg.Add(1)
	go func() {
		defer bg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			lctx, lcancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- replica.Link(lctx, sock) }()
			links.Add(1)
			_ = replica.Write(func(db *sqlpipe.Database) error {
				return PublishDesiredSeat(db, DesiredSeat{Seat: fmt.Sprintf("seat-%d", i%seats), Generation: 1, Revision: int64(i + 1), Provider: "grok", Model: "m", WorkDir: "/tmp/w", ConfigJSON: `{}`})
			})
			time.Sleep(2 * time.Millisecond)
			lcancel()
			if err := <-done; err != nil {
				t.Errorf("link %d: %v", i, err)
				return
			}
		}
	}()
	// A legitimate concurrent reader of the broker's file.
	bg.Add(1)
	go func() {
		defer bg.Done()
		db, err := sqlpipe.OpenDatabase(dbPath)
		if err != nil {
			t.Error(err)
			return
		}
		defer db.Close()
		for ctx.Err() == nil {
			_, _ = db.Query("SELECT COUNT(*) FROM " + UsageTable)
		}
	}()

	var pumpsDone sync.WaitGroup
	for g := 0; g < pumps; g++ {
		pumpsDone.Add(1)
		go func() {
			defer pumpsDone.Done()
			for i := 0; i < each; i++ {
				id := fmt.Sprintf("r%d-%d", g, i)
				claudia.RecordObservedReceipt(&claudia.RequestReceipt{RequestID: id, SessionID: "s", Seat: fmt.Sprintf("pump-%d", g),
					State: claudia.ReceiptAccepted, AttemptID: "a", TurnIDs: []string{}, AcceptedAt: "2026-10-19T00:00:00Z", UpdatedAt: "2026-10-19T00:00:00Z"})
				claudia.RecordObservedUsage(&claudia.UsageFact{Seq: uint64(i + 1), CallID: "c" + id, Seat: fmt.Sprintf("pump-%d", g), SessionID: "s",
					Kind: "model", State: "completed", Provider: "openai", Model: "m",
					Requests:  []claudia.UsageRequestRef{{RequestID: id, AttemptID: "a"}},
					StartedAt: "2026-10-19T00:00:00Z", FinishedAt: "2026-10-19T00:00:01Z",
					CounterSource: "adapter_normalized_positive", Coverage: "engine_invocation"})
			}
		}()
	}
	pumpsDone.Wait()
	cancel()
	bg.Wait()
	if links.Load() < 2 {
		t.Fatalf("only %d links ran alongside the pumps; no handshake raced a publish", links.Load())
	}
	w.mu.Lock()
	refused := append([]string(nil), w.msgs...)
	w.mu.Unlock()
	for _, m := range refused {
		if strings.Contains(m, "not recorded") || strings.Contains(m, "database is locked") {
			t.Errorf("seat-pump write refused: %s", m)
		}
	}
	want := int64(pumps * each)
	ownerRead(t, func(db *sqlpipe.Database) error {
		if u, r := count(t, db, UsageTable), count(t, db, ReceiptTable); u != want || r != want {
			t.Fatalf("broker file holds %d usage facts and %d receipts, want %d each", u, r, want)
		}
		return nil
	})
	// One last link delivers everything to the consumer's own file, and the
	// consumer's seats to the broker's.
	lctx, lcancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- replica.Link(lctx, sock) }()
	linkEnded := func() bool {
		select {
		case err := <-done:
			t.Fatalf("final link ended before the files converged: %v", err)
		default:
		}
		return false
	}
	eventually(t, "every fact at the consumer", func() bool {
		linkEnded()
		var u, r int64
		_ = replica.Read(func(db *sqlpipe.Database) error {
			u, r = count(t, db, UsageTable), count(t, db, ReceiptTable)
			return nil
		})
		return u == want && r == want
	})
	eventually(t, "every desired seat at the broker", func() bool {
		linkEnded()
		var n int64
		ownerRead(t, func(db *sqlpipe.Database) error { n = count(t, db, DesiredSeatTable); return nil })
		return n == min(int64(links.Load()), seats)
	})
	lcancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A consumer finds the control socket from the same setting it finds the
// broker by.
func TestControlSocketFollowsTheBrokerSocket(t *testing.T) {
	t.Setenv(broker.SocketPathEnv, "/tmp/some-broker.sock")
	got, err := claudia.ControlSocket()
	if err != nil || got != "/tmp/some-broker.sock.control" {
		t.Fatalf("ControlSocket = %q %v", got, err)
	}
}
