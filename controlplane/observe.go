// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// ErrNotOwner refuses a writer that is not the process that owns the file.
// claudia-control.db is written by the claudia broker alone (🎯T1074.9: one
// writer per database). A library consumer, such as jevonsd, keeps its own
// file and syncs with the broker over claudia.ControlSocketPath (see Replica).
var ErrNotOwner = errors.New("control plane: this process does not own the Claudia control file")

// ErrOwnerHeld refuses a second owner of one file, in this process or another.
var ErrOwnerHeld = errors.New("control plane: the Claudia control file already has an owner")

// busyTimeoutMS bounds how long a write waits for a reader's shared lock.
// The owner is the only writer; this is for readers that are legitimately
// concurrent (a test, the sqlite3 CLI, a snapshot copy), which in rollback
// journal mode block a commit for the length of their read.
const busyTimeoutMS = 5000

// plane is the owner's one connection to claudia-control.db. Every write,
// whether a seat-pump publish, a reconcile or a peer link applying changes,
// goes through it under mu.
type plane struct {
	mu    sync.Mutex
	db    *sqlpipe.Database
	lock  *os.File
	flush chan struct{}
}

// owned is this process's open owner plane, for ReconcileFile.
var (
	ownedMu   sync.Mutex
	owned     *plane
	ownedPath string
)

// openOwner opens path as its owner. Only the broker daemon process may own
// it (broker.SelfHosted), and only one at a time: an exclusive lock on
// path+".owner", which names the holder's pid, refuses a second owner loudly
// instead of letting two connections race for SQLite's write lock.
func openOwner(path string) (*plane, error) {
	if !broker.SelfHosted() {
		return nil, fmt.Errorf("%w: %s belongs to the claudia broker; a consumer opens its own file with OpenReplica and links to the broker's control socket", ErrNotOwner, path)
	}
	lock, err := os.OpenFile(path+".owner", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path + ".owner")
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s is held by pid %s", ErrOwnerHeld, path, strings.TrimSpace(string(holder)))
		}
		return nil, err
	}
	if err := lock.Truncate(0); err == nil {
		_, _ = lock.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	db, err := openFile(path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &plane{db: db, lock: lock, flush: make(chan struct{}, 1)}, nil
}

// openFile opens one side's own file with the pair schema.
func openFile(path string) (*sqlpipe.Database, error) {
	db, err := sqlpipe.OpenDatabase(path)
	if err != nil {
		return nil, err
	}
	if err := db.Exec("PRAGMA busy_timeout = " + strconv.Itoa(busyTimeoutMS)); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := EnsureSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (p *plane) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db == nil {
		return nil
	}
	err := p.db.Close()
	p.db = nil
	_ = p.lock.Close() // releases the owner lock
	return err
}

// withDB runs fn while every other writer waits, then tells a live peer
// link there may be something to send.
func (p *plane) withDB(fn func(*sqlpipe.Database) error) error {
	p.mu.Lock()
	if p.db == nil {
		p.mu.Unlock()
		return fmt.Errorf("control plane: closed")
	}
	err := fn(p.db)
	p.mu.Unlock()
	select {
	case p.flush <- struct{}{}:
	default:
	}
	return err
}

func (p *plane) publishUsage(fact *claudia.UsageFact) error {
	if fact == nil {
		return fmt.Errorf("control plane: nil usage fact")
	}
	return p.withDB(func(db *sqlpipe.Database) error { return PublishUsageFacts(db, []claudia.UsageFact{*fact}) })
}

func (p *plane) publishReceipt(receipt *claudia.RequestReceipt) error {
	if receipt == nil {
		return fmt.Errorf("control plane: nil receipt")
	}
	return p.withDB(func(db *sqlpipe.Database) error { return PublishReceipt(db, receipt) })
}

// serve answers consumer links on ln, one at a time, until ln closes. The
// broker is the sqlpipe client of each link: it claims the Claudia tables,
// and the consumer approves that claim.
func (p *plane) serve(ln net.Listener, done <-chan struct{}) {
	var active sync.Mutex
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if !active.TryLock() {
			// One consumer writes the Jevons tables. A second link would
			// be a second writer of them.
			slog.Warn("control plane: refused a second consumer link while one is live")
			_ = conn.Close()
			continue
		}
		go func() {
			defer active.Unlock()
			err := runLink(conn, done, linkSide{
				lock:   &p.mu,
				db:     func() *sqlpipe.Database { return p.db },
				flush:  p.flush,
				config: sqlpipe.PeerConfig{OwnedTables: ClaudiaTables()},
				client: true,
			})
			slog.Info("control plane: consumer link ended", "err", err)
		}()
	}
}

// PublishFile opens path as this broker's own control file, creating it
// when it is absent, binds it as the seat-pump publisher, and serves the
// consumer peer link on socket (claudia.ControlSocketPath of the broker
// socket). It refuses unless this
// process is the broker daemon, and refuses a second owner. The returned
// function settles started receipts, stops the link, unbinds the pump
// and releases the file. The broker holds it for the process lifetime.
func PublishFile(path, sock string) (func() error, error) {
	p, err := openOwner(path)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(sock) // the owner lock is held: a socket left here is stale
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = p.close()
		return nil, fmt.Errorf("control plane: peer socket: %w", err)
	}
	_ = os.Chmod(sock, 0o600)
	done := make(chan struct{})
	go p.serve(ln, done)

	ownedMu.Lock()
	owned, ownedPath = p, path
	ownedMu.Unlock()
	claudia.SetControlPlaneObservers(p.publishUsage, p.publishReceipt)
	// The serving sidecar can die while this process stays up (a kill,
	// a crash). Its Wait path is the only observer that still runs.
	// Broker shutdown below still settles anything that exit missed.
	omp.SetServingExitHook(func(int) {
		_ = p.withDB(func(db *sqlpipe.Database) error {
			return SettleStartedObligations(db, SidecarExitReceiptReason)
		})
	})
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() { closeErr = closeOwned(p, ln, sock, done) })
		return closeErr
	}, nil
}

func closeOwned(p *plane, ln net.Listener, sock string, done chan struct{}) error {
	omp.SetServingExitHook(nil)
	// A receipt left started has no later terminal fact once this process is
	// gone. Settle those rows before unbinding so the file a later snapshot
	// reads is not an open obligation. Accepted receipts stay accepted. Usage
	// facts are left as the journal wrote them (see SettleStartedObligations).
	err := p.withDB(func(db *sqlpipe.Database) error {
		return SettleStartedObligations(db, ShutdownReceiptReason)
	})
	claudia.SetControlPlaneObservers(nil, nil)
	close(done)
	_ = ln.Close()
	_ = os.Remove(sock)
	ownedMu.Lock()
	if owned == p {
		owned, ownedPath = nil, ""
	}
	ownedMu.Unlock()
	if cerr := p.close(); err == nil {
		err = cerr
	}
	return err
}
