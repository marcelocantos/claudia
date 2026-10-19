// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// frameProxy sits on the wire between a consumer and the broker's control
// socket. It forwards one peer frame at a time, in the order it read them,
// and records what each carried. A hook may run before a frame is forwarded:
// a write made there lands at that exact step of the link's handshake.
type frameProxy struct {
	t        *testing.T
	ln       net.Listener
	path     string
	upstream string

	mu    sync.Mutex // serialises forwarding, so the frame order is total
	conns []net.Conn
	links int
	log   []frameRecord
	// before runs, with mu held, before frame n of link l is forwarded.
	before func(link, n int, toConsumer bool, msg sqlpipe.PeerMessage)
}

type frameRecord struct {
	link       int
	toConsumer bool
	msg        sqlpipe.PeerMessage
}

func startFrameProxy(t *testing.T, upstream string) *frameProxy {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cpx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "proxy.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	p := &frameProxy{t: t, ln: ln, path: path, upstream: upstream}
	t.Cleanup(func() { p.close() })
	go p.serve()
	return p
}

func (p *frameProxy) serve() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("unix", p.upstream)
		if err != nil {
			_ = down.Close()
			continue
		}
		p.mu.Lock()
		link := p.links
		p.links++
		p.conns = append(p.conns, down, up)
		p.mu.Unlock()
		n := 0 // frames forwarded on this link, both directions
		relay := func(from, to net.Conn, toConsumer bool) {
			defer func() { _ = from.Close(); _ = to.Close() }()
			for {
				b, err := readFrame(from)
				if err != nil {
					return
				}
				msg, err := sqlpipe.DeserializePeer(b)
				if err != nil {
					p.t.Errorf("proxy: undecodable frame: %v", err)
					return
				}
				p.mu.Lock()
				if p.before != nil {
					p.before(link, n, toConsumer, msg)
				}
				n++
				p.log = append(p.log, frameRecord{link: link, toConsumer: toConsumer, msg: msg})
				err = writeFrame(to, b)
				p.mu.Unlock()
				if err != nil {
					return
				}
			}
		}
		go relay(up, down, true)
		go relay(down, up, false)
	}
}

// drop ends every link through the proxy, as a broken wire would.
func (p *frameProxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *frameProxy) close() {
	_ = p.ln.Close()
	p.drop()
}

func (p *frameProxy) linkCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.links
}

// carried counts the frames that carried needle in a row payload (a
// handshake diff or a changeset) in the given direction.
func (p *frameProxy) carried(needle string, toConsumer bool) (n int, where []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.log {
		if r.toConsumer != toConsumer {
			continue
		}
		var data []byte
		var label string
		switch m := r.msg.Payload.(type) {
		case sqlpipe.DiffReadyMsg:
			data, label = m.Patchset, fmt.Sprintf("link %d DiffReady seq=%d", r.link, m.Seq)
		case sqlpipe.ChangesetMsg:
			data, label = m.Data, fmt.Sprintf("link %d Changeset seq=%d", r.link, m.Seq)
		}
		if bytes.Contains(data, []byte(needle)) {
			n++
			where = append(where, label)
		}
	}
	return n, where
}

// framesOn counts the frames forwarded on one link, both directions.
func (p *frameProxy) framesOn(link int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.log {
		if r.link == link {
			n++
		}
	}
	return n
}

// deliveryBudget bounds how long a live link may take to deliver one write.
// Delivery is a frame or two on a local socket; a write still missing after
// this is not coming without a reconnect.
const deliveryBudget = 10 * time.Second

func soon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(deliveryBudget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, deliveryBudget)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func usageFact(seat, callID string) *claudia.UsageFact {
	return &claudia.UsageFact{Seq: 1, CallID: callID, Seat: seat, SessionID: "s",
		Kind: "model", State: "completed", Provider: "openai", Model: "m",
		Requests:  []claudia.UsageRequestRef{{RequestID: "r-" + callID, AttemptID: "a"}},
		StartedAt: "2026-10-19T00:00:00Z", FinishedAt: "2026-10-19T00:00:01Z",
		CounterSource: "adapter_normalized_positive", Coverage: "engine_invocation"}
}

func consumerHasCall(t *testing.T, r *Replica, callID string) bool {
	t.Helper()
	var n int64
	if err := r.Read(func(db *sqlpipe.Database) error {
		qr, err := db.Query("SELECT COUNT(*) FROM "+UsageTable+" WHERE call_id = ?", callID)
		if err != nil {
			return err
		}
		n = qr.Rows[0][0].(int64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func brokerHasSeat(t *testing.T, seat string) bool {
	t.Helper()
	var n int64
	ownerRead(t, func(db *sqlpipe.Database) error {
		qr, err := db.Query("SELECT COUNT(*) FROM "+DesiredSeatTable+" WHERE seat = ?", seat)
		if err != nil {
			return err
		}
		n = qr.Rows[0][0].(int64)
		return nil
	})
	return n == 1
}

// runningLink is one Replica.Link through the proxy.
type runningLink struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// linkOnce starts one Replica.Link to sock. Cleanup stops it.
func linkOnce(t *testing.T, r *Replica, sock string) *runningLink {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &runningLink{cancel: cancel, done: make(chan struct{})}
	go func() {
		l.err = r.Link(ctx, sock)
		close(l.done)
	}()
	t.Cleanup(func() { _ = l.stop() })
	return l
}

// stop ends the link and returns how Link returned.
func (l *runningLink) stop() error {
	l.cancel()
	<-l.done
	return l.err
}

// wait returns how Link returned once it has, without asking it to stop.
func (l *runningLink) wait() error {
	<-l.done
	return l.err
}

// requireUp fails when the link has already ended.
func (l *runningLink) requireUp(t *testing.T, what string) {
	t.Helper()
	select {
	case <-l.done:
		t.Fatalf("%s: the link ended (%v); a live link must not need a redial", what, l.err)
	default:
	}
}

// handshakeWrites runs one link scenario. With redial, a first link carries
// the starting rows, the wire is cut, both sides commit while it is down,
// and the consumer dials again; otherwise the first link is the one under
// test. Just before frame k of the link under test is forwarded (k < 0:
// never), the broker records a usage fact and the consumer a desired seat.
// It returns how many frames that link exchanged before it went quiet.
func handshakeWrites(t *testing.T, k int, redial bool) int {
	t.Helper()
	dbPath, sock := brokerDirs(t)
	closeFn := publishOwned(t, dbPath, sock)
	defer func() { _ = closeFn() }()
	proxy := startFrameProxy(t, sock)
	r, err := OpenReplica(filepath.Join(t.TempDir(), JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	consumerWrite := func(seat string) {
		t.Helper()
		if err := r.Write(func(db *sqlpipe.Database) error { return PublishDesiredSeat(db, desired(seat, 1, 1)) }); err != nil {
			t.Fatal(err)
		}
	}
	// Rows each side holds and the other lacks, so the handshake runs a
	// real diff in both directions.
	claudia.RecordObservedUsage(usageFact("seat-0", "call-before"))
	consumerWrite("seat-before")

	underTest := 0
	if redial {
		first := linkOnce(t, r, proxy.path)
		soon(t, "call-before on the first link", func() bool {
			first.requireUp(t, "call-before")
			return consumerHasCall(t, r, "call-before")
		})
		soon(t, "seat-before on the first link", func() bool {
			first.requireUp(t, "seat-before")
			return brokerHasSeat(t, "seat-before")
		})
		proxy.drop()
		_ = first.wait() // a cut wire; how Link reports it is not under test
		claudia.RecordObservedUsage(usageFact("seat-0", "call-down"))
		consumerWrite("seat-down")
		underTest = 1
	}

	callID, seat := fmt.Sprintf("call-hs-%d", k), fmt.Sprintf("seat-hs-%d", k)
	fired := false
	proxy.mu.Lock()
	proxy.before = func(link, n int, _ bool, _ sqlpipe.PeerMessage) {
		if link != underTest || n != k || fired {
			return
		}
		fired = true
		claudia.RecordObservedUsage(usageFact("seat-1", callID))
		if err := r.Write(func(db *sqlpipe.Database) error { return PublishDesiredSeat(db, desired(seat, 1, 1)) }); err != nil {
			t.Error(err)
		}
	}
	proxy.mu.Unlock()
	link := linkOnce(t, r, proxy.path)
	want := []string{"call-before", "seat-before"}
	if redial {
		want = append(want, "call-down", "seat-down")
	}
	for _, row := range want {
		soon(t, row+" on the link under test", func() bool {
			link.requireUp(t, row)
			if row[:4] == "call" {
				return consumerHasCall(t, r, row)
			}
			return brokerHasSeat(t, row)
		})
	}
	// The handshake is over once no frame has moved for a while.
	frames := -1
	for frames != proxy.framesOn(underTest) {
		frames = proxy.framesOn(underTest)
		time.Sleep(100 * time.Millisecond)
	}
	if k < 0 {
		return frames
	}
	proxy.mu.Lock()
	didFire := fired
	proxy.mu.Unlock()
	if !didFire {
		t.Fatalf("the handshake ended after %d frames, before frame %d", frames, k)
	}

	soon(t, callID+" at the consumer", func() bool {
		link.requireUp(t, callID)
		return consumerHasCall(t, r, callID)
	})
	soon(t, seat+" at the broker", func() bool {
		link.requireUp(t, seat)
		return brokerHasSeat(t, seat)
	})
	// The link is live: later writes on both sides flow on it.
	claudia.RecordObservedUsage(usageFact("seat-2", "call-after"))
	consumerWrite("seat-after")
	soon(t, "call-after on the live link", func() bool {
		link.requireUp(t, "call-after")
		return consumerHasCall(t, r, "call-after")
	})
	soon(t, "seat-after on the live link", func() bool {
		link.requireUp(t, "seat-after")
		return brokerHasSeat(t, "seat-after")
	})
	if err := link.stop(); err != nil {
		t.Fatalf("link under test: %v", err)
	}
	if n := proxy.linkCount(); n != underTest+1 {
		t.Fatalf("%d links, want %d: the writes needed a redial", n, underTest+1)
	}
	for _, c := range []struct {
		needle     string
		toConsumer bool
	}{{callID, true}, {seat, false}, {"call-after", true}, {"seat-after", false}} {
		if n, where := proxy.carried(c.needle, c.toConsumer); n != 1 {
			t.Errorf("%s crossed the wire %d times (%v), want exactly once", c.needle, n, where)
		}
	}
	return frames
}

// A write committed on either side while a fresh link is mid-handshake is
// delivered exactly once, and the link stays up. Before the sqlpipe fix the
// handshake diff carried the row and the first live changeset carried it
// again; the receiving peer's apply aborted on the duplicate
// ("changeset_apply: not an error") and the link dropped.
func TestLinkDeliversAWriteMadeAtEveryHandshakeStepOnce(t *testing.T) {
	asBroker(t)
	steps := handshakeWrites(t, -1, false)
	if steps < 8 {
		t.Fatalf("a handshake of %d frames is not a two-way diff", steps)
	}
	for k := range steps {
		t.Run(fmt.Sprintf("before_frame_%d", k), func(t *testing.T) { handshakeWrites(t, k, false) })
	}
}

// After the wire is cut and the consumer dials again, a write at any step of
// the new handshake is delivered once and later writes keep flowing on that
// link, with no further reconnect.
func TestLinkKeepsFlowingAfterADropAndRedial(t *testing.T) {
	asBroker(t)
	steps := handshakeWrites(t, -1, true)
	if steps < 8 {
		t.Fatalf("a handshake of %d frames is not a two-way diff", steps)
	}
	for k := range steps {
		t.Run(fmt.Sprintf("before_frame_%d", k), func(t *testing.T) { handshakeWrites(t, k, true) })
	}
}
