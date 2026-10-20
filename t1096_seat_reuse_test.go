// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// seatKeepingSidecar is a fake that keeps seats loaded across connections,
// as the real sidecar does: a host that closes its connection leaves the
// seat loaded, a load of a loaded seat under another session is refused
// with seat_identity_mismatch (server.ts, since 34da965), and a drop
// unloads it — refusing when it names another session.
type seatKeepingSidecar struct {
	mu    sync.Mutex
	seats map[string]string // seat -> session
	ops   []omp.Message
	// dropped receives each seat a drop unloaded.
	dropped chan string
}

func (f *seatKeepingSidecar) seen() []omp.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]omp.Message(nil), f.ops...)
}

func (f *seatKeepingSidecar) session(seat string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.seats[seat]
	return s, ok
}

func (f *seatKeepingSidecar) answer(msg omp.Message) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, msg)
	switch msg.Op {
	case omp.OpLoad:
		if cur, ok := f.seats[msg.Seat]; ok {
			if msg.SessionID != "" && cur != msg.SessionID {
				return map[string]any{"seat": msg.Seat, "type": "error", "reason": "seat_identity_mismatch", "text": "load seat identity mismatch"}
			}
			return map[string]any{"seat": msg.Seat, "type": "ready", "how": "adopted", "resume_checked": msg.RequireResume}
		}
		f.seats[msg.Seat] = msg.SessionID
		return map[string]any{"seat": msg.Seat, "type": "ready", "how": "launched", "resume_checked": msg.RequireResume}
	case omp.OpDrop:
		cur, ok := f.seats[msg.Seat]
		if !ok {
			return map[string]any{"seat": msg.Seat, "type": "seat_error", "text": "seat is not loaded"}
		}
		if msg.SessionID != "" && cur != msg.SessionID {
			return map[string]any{"seat": msg.Seat, "type": "seat_error", "text": "request seat identity mismatch"}
		}
		delete(f.seats, msg.Seat)
		select {
		case f.dropped <- msg.Seat:
		default:
		}
		return map[string]any{"seat": msg.Seat, "type": "dropped"}
	}
	return nil
}

func startSeatKeepingSidecar(t *testing.T, preloaded map[string]string) *seatKeepingSidecar {
	t.Helper()
	f := &seatKeepingSidecar{seats: map[string]string{}, dropped: make(chan string, 8)}
	for k, v := range preloaded {
		f.seats[k] = v
	}
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-t1096-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var msg omp.Message
					if json.Unmarshal(line, &msg) != nil {
						continue
					}
					if ev := f.answer(msg); ev != nil {
						b, _ := json.Marshal(ev)
						_, _ = c.Write(append(b, '\n'))
					}
				}
			}()
		}
	}()
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)
	stubOMPPlans(t)
	return f
}

func opsOf(ops []omp.Message, op string) []omp.Message {
	var out []omp.Message
	for _, m := range ops {
		if m.Op == op {
			out = append(out, m)
		}
	}
	return out
}

// jevons 🎯T1096 (a): removing a seat — what jevons_agent_kill and the finished-work
// reap do — unloads it from the sidecar, so the name is free: the same name
// starts again on another provider under a fresh session.
func TestT1096RemovedSeatIsUnloadedAndItsNameStartsAgain(t *testing.T) {
	fake := startSeatKeepingSidecar(t, nil)
	reg, _ := switchRegistry(t)
	if err := reg.Register(AgentDef{Name: "jv-t1104-auto", Provider: Provider(omp.Anthropic), WorkDir: t.TempDir(), SessionID: "s-killed"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("jv-t1104-auto")
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove("jv-t1104-auto"); err != nil {
		t.Fatal(err)
	}
	// The drop is sent as the handle closes, and the fake reads it on its
	// own goroutine; go test -timeout is the clock if it never comes.
	<-fake.dropped
	drops := opsOf(fake.seen(), omp.OpDrop)
	if len(drops) != 1 || drops[0].SessionID != "s-killed" {
		t.Fatalf("drops = %+v, want one drop naming the removed seat's own session", drops)
	}

	if err := reg.Register(AgentDef{Name: "jv-t1104-auto", Provider: Provider(omp.OpenAICodex), WorkDir: t.TempDir(), SessionID: "s-remint"}); err != nil {
		t.Fatal(err)
	}
	again, err := reg.Launch("jv-t1104-auto")
	if err != nil {
		t.Fatalf("re-start of a removed name failed: %v", err)
	}
	defer reg.Stop("jv-t1104-auto")
	if again.Provider() != Provider(omp.OpenAICodex) {
		t.Fatalf("re-start provider = %s, want openai-codex", again.Provider())
	}
	if s, _ := fake.session("jv-t1104-auto"); s != "s-remint" {
		t.Fatalf("sidecar holds session %q, want the re-start's s-remint", s)
	}
}

// jevons 🎯T1096 (b): a fresh start whose name is still loaded in the sidecar under
// another session — a seat whose removal never reached the sidecar (a host
// that lost its handle, a sidecar that predates the drop) — replaces the
// stale seat and starts. The stale conversation stays on disk under its own
// session (omp-seats/<seat>/<session>.jsonl); only the loaded seat goes.
func TestT1096FreshStartReplacesAStaleLoadedSeat(t *testing.T) {
	fake := startSeatKeepingSidecar(t, map[string]string{"jv-t1104-auto": "s-stale"})
	reg, _ := switchRegistry(t)
	if err := reg.Register(AgentDef{Name: "jv-t1104-auto", Provider: Provider(omp.OpenAICodex), WorkDir: t.TempDir(), SessionID: "s-fresh"}); err != nil {
		t.Fatal(err)
	}
	proc, err := reg.Launch("jv-t1104-auto")
	if err != nil {
		t.Fatalf("fresh start under a stale loaded name failed: %v", err)
	}
	defer reg.Stop("jv-t1104-auto")
	if proc.SessionID() != "s-fresh" || proc.Provider() != Provider(omp.OpenAICodex) {
		t.Fatalf("started %s on session %q, want openai-codex on s-fresh", proc.Provider(), proc.SessionID())
	}
	ops := fake.seen()
	if len(ops) != 3 || ops[0].Op != omp.OpLoad || ops[1].Op != omp.OpDrop || ops[2].Op != omp.OpLoad {
		t.Fatalf("ops = %+v, want load (refused), drop, load", ops)
	}
	if ops[1].Seat != "jv-t1104-auto" || ops[2].SessionID != "s-fresh" {
		t.Fatalf("drop %+v / reload %+v", ops[1], ops[2])
	}
}

// jevons 🎯T1096 (c): a start that requires its existing conversation does not
// replace whatever is loaded under its name. It is refused, and the refusal
// names the seat, the session it asked for, and how to clear the name — not
// "differs from registered provider or purpose".
func TestT1096ResumeAgainstAStaleLoadedSeatIsRefusedReadably(t *testing.T) {
	fake := startSeatKeepingSidecar(t, map[string]string{"w": "s-stale"})
	_, err := startDirectContext(context.Background(), Config{
		Provider: Provider(omp.Anthropic), Name: "w", WorkDir: t.TempDir(), TermLogPath: "-",
		SessionID: "s-durable", RequireResume: true,
	})
	if !errors.Is(err, ErrSeatIdentityMismatch) {
		t.Fatalf("err = %v, want ErrSeatIdentityMismatch", err)
	}
	for _, want := range []string{`"w"`, "s-durable", "another session", "remove"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "provider or purpose") {
		t.Fatalf("refusal %q still blames provider or purpose", err)
	}
	if drops := opsOf(fake.seen(), omp.OpDrop); len(drops) != 0 {
		t.Fatalf("a resume dropped the loaded seat: %+v", drops)
	}
	if s, _ := fake.session("w"); s != "s-stale" {
		t.Fatalf("loaded seat = %q, want s-stale left alone", s)
	}
}
