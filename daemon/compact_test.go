// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	claudia "github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

func TestT244BrokerCompactPropagatesConnectionCancel(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	var sawCtx context.Context
	f.startHook = func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		return claudia.StartStub(ctx, cfg, &claudia.StubAgentOps{
			Compact: func(ctx context.Context, _ int) (claudia.SeatCompact, error) {
				sawCtx = ctx
				close(started)
				<-ctx.Done()
				return claudia.SeatCompact{}, ctx.Err()
			},
		})
	}
	f.boot(t, nil)
	conn := rawSeat(t, f.sock, "compact-cancel")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = conn.WriteRequest(&broker.Request{ID: "compact", Type: broker.TypeCompact, Compact: &broker.CompactRequest{
			Name: "compact-cancel", BoundTokens: 180_000,
		}})
		for {
			if _, err := conn.ReadResponse(); err != nil {
				return
			}
		}
	}()
	waitFor(t, "compact started", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})
	_ = conn.Close()
	wg.Wait()
	waitFor(t, "compact ctx canceled", func() bool {
		return sawCtx != nil && sawCtx.Err() != nil
	})
	if !errors.Is(sawCtx.Err(), context.Canceled) {
		t.Fatalf("compact ctx err = %v, want context.Canceled after disconnect", sawCtx.Err())
	}
}

// TestT244BrokerDisconnectDuringSidecarFold is the clause-3 oracle: broker
// connection loss during a real sidecar bound-compact fold must emit matching
// compact_abort, leave durable history without a new compaction entry, and
// return a typed cancel failure to the caller.
func TestT244BrokerDisconnectDuringSidecarFold(t *testing.T) {
	_, root, _, tracePath := bootT244SidecarDaemon(t)
	seat := "t244-fold"
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1200)
	session := "gen-t244"
	storePath := filepath.Join(root, "omp-seats", seat, session+".jsonl")
	writePreloadedT244Store(t, storePath, seat, session, chunk)
	conn := grantOMPSeat(t, brokerSocket(t), seat, workDir, session)
	resp := rawCall(t, conn, &broker.Request{
		ID: "wake", Type: broker.TypeSend,
		Send: &broker.SendRequest{Name: seat, Text: "wake after preload"},
	})
	if resp.Type == broker.TypeError {
		t.Fatalf("wake send: %+v", resp.Error)
	}
	waitFor(t, "wake prompt on sidecar", func() bool {
		return strings.Count(readFileOrEmpty(tracePath), `"op":"prompt"`) >= 1
	})
	time.Sleep(500 * time.Millisecond)
	before := countStoreType(t, storePath, "compaction")

	var wg sync.WaitGroup
	var compactResp *broker.Response
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := conn.WriteRequest(&broker.Request{
			ID: "compact-fold", Type: broker.TypeCompact,
			Compact: &broker.CompactRequest{Name: seat, BoundTokens: 180_000},
		}); err != nil {
			t.Error(err)
			return
		}
		_ = conn.SetDeadline(time.Now().Add(90 * time.Second))
		for {
			resp, err := conn.ReadResponse()
			if err != nil {
				return
			}
			if resp.ID == "compact-fold" {
				compactResp = resp
				return
			}
		}
	}()

	var compactReqID string
	waitFor(t, "sidecar compact op on wire", func() bool {
		compactReqID = lastTraceOp(tracePath, "compact")
		return compactReqID != ""
	})
	time.Sleep(200 * time.Millisecond)
	_ = conn.Close()
	waitFor(t, "compact_abort on wire", func() bool {
		return traceHasOp(tracePath, "compact_abort", compactReqID)
	})
	wg.Wait()

	if !traceHasOp(tracePath, "compact_abort", compactReqID) {
		t.Fatalf("missing compact_abort for compact request_id %q; trace:\n%s", compactReqID, readFileOrEmpty(tracePath))
	}
	after := countStoreType(t, storePath, "compaction")
	if after > before {
		t.Fatalf("compaction entries %d -> %d after broker disconnect", before, after)
	}
	if compactResp != nil && compactResp.Type != broker.TypeError {
		t.Fatalf("compact response type = %s, want error when present", compactResp.Type)
	}
	if compactResp != nil && compactResp.Error != nil {
		msg := compactResp.Error.Message
		if !strings.Contains(strings.ToLower(msg), "cancel") && !errors.Is(parseCompactBrokerErr(msg), claudia.ErrCompactCanceled) {
			t.Fatalf("compact failure = %q, want cancel typed outcome", msg)
		}
	}
}

var t244BrokerSock string

func brokerSocket(t *testing.T) string {
	t.Helper()
	if t244BrokerSock == "" {
		t.Fatal("bootT244SidecarDaemon not called")
	}
	return t244BrokerSock
}

func bootT244SidecarDaemon(t *testing.T) (*fixture, string, string, string) {
	t.Helper()
	f := newFixture(t)
	root := filepath.Dir(f.sock)
	ompSock := filepath.Join(root, "omp.sock")
	tracePath := filepath.Join(root, "omp-trace.jsonl")
	t244BrokerSock = f.sock

	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := filepath.Abs("../sidecar/testdata/t244-compact-provider.ts")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bun, entry, ompSock, "--lifeline=stdin")
	cmd.Env = append(omp.ScrubEnv(os.Environ()),
		"HOME="+root,
		"CLAUDIA_OMP_SPOOL_DIR="+filepath.Join(root, "spool"),
		"CLAUDIA_TEST_COMPACT_HOLD=1",
		"CLAUDIA_TEST_COMPACT_SOFT_ONLY=1",
		"CLAUDIA_TEST_OMP_TRACE="+tracePath,
	)
	cmd.Stderr = os.Stderr
	life, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = life.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ctx := t.Context()
	waitFor(t, "t244 sidecar listening", func() bool { return omp.Listening(ctx, ompSock) })

	t.Setenv(omp.SocketEnv, ompSock)
	t.Setenv("CLAUDIA_OMP_SPOOL_DIR", filepath.Join(root, "spool"))
	t.Setenv("CLAUDIA_OMP_NO_REFRESH", "1")
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	item := omp.Item{Records: map[string]omp.Record{}}
	for _, p := range []string{omp.Anthropic, omp.OpenAICodex, omp.XAIOAuth, omp.Cursor} {
		item.Records[p] = omp.Record{AccessToken: "fixture", RefreshToken: "fixture", Expiry: time.Now().Add(time.Hour)}
	}
	raw, _ := json.Marshal(item)
	if err := omp.Open(ctx, omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) { return raw, nil }}); err != nil {
		t.Fatal(err)
	}
	opts := f.options(nil)
	opts.DisableResume = true
	opts.launchers = &claudia.RegistryLaunchers{Start: claudia.StartDirectContext, Adopt: claudia.Adopt}
	f.bootWith(t, opts)
	return f, root, ompSock, tracePath
}

func grantOMPSeat(t *testing.T, sock, name, workDir, session string) *broker.Conn {
	t.Helper()
	c, err := broker.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	def, err := claudia.EncodeGrantDefinition(claudia.GrantDefinition{AgentDef: claudia.AgentDef{
		Name: name, WorkDir: workDir, SessionID: session, TermLogPath: "-",
		Provider: claudia.Provider(omp.Anthropic), Model: "claude-sonnet-5",
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := rawCall(t, c, &broker.Request{ID: "grant", Type: broker.TypeGrant,
		Grant: &broker.GrantRequest{Name: name, Def: def}})
	if resp.Type != broker.TypeGranted {
		t.Fatalf("grant answered %s: %+v", resp.Type, resp.Error)
	}
	return c
}

type brokerDemuxConn struct {
	*broker.Conn
	mu      sync.Mutex
	waiters map[string]chan *broker.Response
}

func startBrokerDemux(t *testing.T, c *broker.Conn) *brokerDemuxConn {
	t.Helper()
	d := &brokerDemuxConn{Conn: c, waiters: map[string]chan *broker.Response{}}
	go func() {
		for {
			resp, err := d.Conn.ReadResponse()
			if err != nil {
				return
			}
			d.mu.Lock()
			ch := d.waiters[resp.ID]
			d.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
	}()
	return d
}

func (d *brokerDemuxConn) call(t *testing.T, req *broker.Request) *broker.Response {
	t.Helper()
	ch := make(chan *broker.Response, 1)
	d.mu.Lock()
	d.waiters[req.ID] = ch
	d.mu.Unlock()
	if err := d.WriteRequest(req); err != nil {
		t.Fatal(err)
	}
	// Block on the broker response; go test -timeout is the only clock (🎯T97).
	return <-ch
}

type traceRec struct {
	Op        string `json:"op"`
	RequestID string `json:"request_id"`
}

func lastTraceOp(path, op string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec traceRec
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Op == op && rec.RequestID != "" {
			last = rec.RequestID
		}
	}
	return last
}

func traceHasOp(path, op, requestID string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec traceRec
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Op == op && rec.RequestID == requestID {
			return true
		}
	}
	return false
}

func countStoreType(t *testing.T, path, typ string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
 needle := `"type":"` + typ + `"`
	return strings.Count(string(b), needle)
}

func readFileOrEmpty(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func parseCompactBrokerErr(msg string) error {
	if strings.Contains(msg, claudia.ErrCompactCanceled.Error()) {
		return claudia.ErrCompactCanceled
	}
	return errors.New(msg)
}

func sendSidecarCompactAbort(t *testing.T, ompSock, seat, requestID string) {
	t.Helper()
	if requestID == "" {
		return
	}
	c, err := net.Dial("unix", ompSock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = fmt.Fprintf(c, `{"op":"compact_abort","seat":%q,"request_id":%q}`+"\n", seat, requestID)
}

func writePreloadedT244Store(t *testing.T, path, seat, session, chunk string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"claudia_seat","v":1,"seat":%q,"session":%q}`+"\n", seat, session)
	for i := range 16 {
		user := fmt.Sprintf(`{"type":"message","id":"u%d","parentId":null,"timestamp":"2026-10-31T00:00:%02d.000Z","message":{"role":"user","content":%q,"timestamp":%d}}`+"\n",
			i, i, fmt.Sprintf("%d: %s", i, chunk), i*2)
		asst := fmt.Sprintf(`{"type":"message","id":"a%d","parentId":"u%d","timestamp":"2026-10-31T00:00:%02d.500Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-sonnet-5","timestamp":%d,"usage":{"input":12000,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":12001,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop"}}`+"\n",
			i, i, i, i*2+1)
		b.WriteString(user)
		b.WriteString(asst)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bootT244InterleaveSidecarDaemon(t *testing.T) (*fixture, string, string, string) {
	t.Helper()
	f := newFixture(t)
	root := filepath.Dir(f.sock)
	ompSock := filepath.Join(root, "omp.sock")
	tracePath := filepath.Join(root, "omp-trace.jsonl")
	t244BrokerSock = f.sock

	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := filepath.Abs("../sidecar/testdata/t244-interleave-provider.ts")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bun, entry, ompSock, "--lifeline=stdin")
	cmd.Env = append(omp.ScrubEnv(os.Environ()),
		"HOME="+root,
		"CLAUDIA_OMP_SPOOL_DIR="+filepath.Join(root, "spool"),
		"CLAUDIA_TEST_OMP_TRACE="+tracePath,
	)
	cmd.Stderr = os.Stderr
	life, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = life.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ctx := t.Context()
	waitFor(t, "t244 interleave sidecar listening", func() bool { return omp.Listening(ctx, ompSock) })

	t.Setenv(omp.SocketEnv, ompSock)
	t.Setenv("CLAUDIA_OMP_SPOOL_DIR", filepath.Join(root, "spool"))
	t.Setenv("CLAUDIA_OMP_NO_REFRESH", "1")
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	item := omp.Item{Records: map[string]omp.Record{}}
	for _, p := range []string{omp.Anthropic, omp.OpenAICodex, omp.XAIOAuth, omp.Cursor} {
		item.Records[p] = omp.Record{AccessToken: "fixture", RefreshToken: "fixture", Expiry: time.Now().Add(time.Hour)}
	}
	raw, _ := json.Marshal(item)
	if err := omp.Open(ctx, omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) { return raw, nil }}); err != nil {
		t.Fatal(err)
	}
	opts := f.options(nil)
	opts.DisableResume = true
	opts.launchers = &claudia.RegistryLaunchers{Start: claudia.StartDirectContext, Adopt: claudia.Adopt}
	f.bootWith(t, opts)
	return f, root, ompSock, tracePath
}

// TestT244BrokerCompactRefusedDuringInFlightPrompt is the broker wire oracle for
// compact while a normal prompt turn is open on production server.ts.
func TestT244BrokerCompactRefusedDuringInFlightPrompt(t *testing.T) {
	_, root, _, _ := bootT244InterleaveSidecarDaemon(t)
	seat := "t244-broker-compact-race"
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	conn := grantOMPSeat(t, brokerSocket(t), seat, workDir, "sess-race")
	dmx := startBrokerDemux(t, conn)
	go func() {
		_ = dmx.call(t, &broker.Request{
			ID: "hold", Type: broker.TypeSend,
			Send: &broker.SendRequest{Name: seat, Text: "HOLD broker prompt"},
		})
	}()
	waitFor(t, "hold prompt started", func() bool {
		info := dmx.call(t, &broker.Request{ID: "info", Type: broker.TypeAgentInfo, AgentInfo: &broker.NamedRequest{Name: seat}})
		return info.Type == broker.TypeAgentInfoResult && info.AgentInfo != nil && info.AgentInfo.PromptInFlight
	})
	before := countStoreType(t, filepath.Join(root, "omp-seats", seat, "sess-race.jsonl"), "compaction")
	resp := dmx.call(t, &broker.Request{
		ID: "compact", Type: broker.TypeCompact,
		Compact: &broker.CompactRequest{Name: seat, BoundTokens: 180_000},
	})
	if resp.Type != broker.TypeError {
		t.Fatalf("compact during prompt type = %s, want error", resp.Type)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Message, claudia.ErrTurnInFlight.Error()) {
		t.Fatalf("compact during prompt = %+v, want turn_in_flight", resp.Error)
	}
	if countStoreType(t, filepath.Join(root, "omp-seats", seat, "sess-race.jsonl"), "compaction") > before {
		t.Fatal("compact during prompt must not commit")
	}
	_ = dmx.call(t, &broker.Request{ID: "abort", Type: broker.TypeInterrupt, Interrupt: &broker.NamedRequest{Name: seat}})
}

// TestT244BrokerSendRefusedDuringHeldBoundCompact is the consumer no-slip oracle:
// a caller-held next large prompt must not pass while bound compact is in flight.
func TestT244BrokerSendRefusedDuringHeldBoundCompact(t *testing.T) {
	_, root, ompSock, tracePath := bootT244InterleaveSidecarDaemon(t)
	seat := "t244-broker-send-slip"
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chunk := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1200)
	session := "sess-heavy"
	storePath := filepath.Join(root, "omp-seats", seat, session+".jsonl")
	writePreloadedT244Store(t, storePath, seat, session, chunk)
	conn := grantOMPSeat(t, brokerSocket(t), seat, workDir, session)
	dmx := startBrokerDemux(t, conn)
	wake := dmx.call(t, &broker.Request{ID: "wake", Type: broker.TypeSend, Send: &broker.SendRequest{Name: seat, Text: "wake"}})
	if wake.Type == broker.TypeError {
		t.Fatalf("wake: %+v", wake.Error)
	}
	waitFor(t, "wake settled", func() bool {
		info := dmx.call(t, &broker.Request{ID: "info2", Type: broker.TypeAgentInfo, AgentInfo: &broker.NamedRequest{Name: seat}})
		return info.Type == broker.TypeAgentInfoResult && info.AgentInfo != nil && !info.AgentInfo.PromptInFlight
	})
	before := countStoreType(t, storePath, "compaction")
	compactDone := make(chan struct{})
	go func() {
		defer close(compactDone)
		_ = dmx.call(t, &broker.Request{
			ID: "compact-hold", Type: broker.TypeCompact,
			Compact: &broker.CompactRequest{Name: seat, BoundTokens: 180_000},
		})
	}()
	waitFor(t, "compact on wire", func() bool {
		return lastTraceOp(tracePath, "compact") != ""
	})
	slip := dmx.call(t, &broker.Request{
		ID: "slip", Type: broker.TypeSend,
		Send: &broker.SendRequest{Name: seat, Text: strings.Repeat("NEXT-LARGE ", 900)},
	})
	if slip.Type != broker.TypeError {
		t.Fatalf("send during compact type = %s, want error", slip.Type)
	}
	if slip.Error == nil || !strings.Contains(slip.Error.Message, claudia.ErrTurnInFlight.Error()) {
		t.Fatalf("send during compact = %+v, want turn_in_flight", slip.Error)
	}
	if countStoreType(t, storePath, "compaction") > before {
		t.Fatal("held compact must not commit before slip refused")
	}
	compactReq := lastTraceOp(tracePath, "compact")
	sendSidecarCompactAbort(t, ompSock, seat, compactReq)
	<-compactDone
}
