// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type t147Log struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *t147Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *t147Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func t147Tool(name string) string {
	return `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"` + name + `","inputSchema":{"type":"object"}}]}}`
}

// 🎯T147: a seat launch builds its host tool list from the servers that
// answer, within a short budget. A stalled server does not hold it up, and
// every server that gives no tools is logged once with its name and why. A
// stalled server's answer, when it comes, is ready for the next launch.
func TestT147StalledOrBrokenHostServersDoNotHoldUpALaunch(t *testing.T) {
	logs := &t147Log{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	serve := func(h http.HandlerFunc) string {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		t.Cleanup(func() { hostToolsCache.Delete(srv.URL) })
		return srv.URL
	}
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	healthy := serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, t147Tool("healthy_tool"))
	})
	stalled := serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		<-release
		_, _ = io.WriteString(w, t147Tool("stalled_tool"))
	})
	rpc := serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32002,"message":"server not initialized"}}`)
	})
	broken := serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	})
	// A port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + ln.Addr().String() + "/mcp"
	ln.Close()
	t.Cleanup(func() { hostToolsCache.Delete(refused) })

	servers := []MCPServer{
		{Name: "healthy", Type: "http", URL: healthy},
		{Name: "stalled", Type: "http", URL: stalled},
		{Name: "rpc", Type: "http", URL: rpc},
		{Name: "broken", Type: "http", URL: broken},
		{Name: "refused", Type: "http", URL: refused},
	}
	start := time.Now()
	_, routes, _ := hostToolsNamed(context.Background(), servers)
	// 🎯T97 exemption: the elapsed launch time is the verdict here — a
	// stalled server must not hold the launch past the budget. The margin
	// only absorbs scheduling on a loaded host.
	if took := time.Since(start); took > hostToolsBudget+3*time.Second {
		t.Fatalf("launch waited %s on a stalled server (budget %s)", took, hostToolsBudget)
	}
	if len(routes) != 1 || routes["healthy_tool"] != healthy {
		t.Fatalf("routes = %v, want only the healthy server's tool", routes)
	}
	out := logs.String()
	for _, want := range []string{
		`server=rpc`, `cause="rpc error"`,
		`server=broken`, `cause="http status"`,
		`server=refused`, `cause="connect refused"`,
		`servers=stalled`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}

	// The stalled server answers late; its tools are there for the next launch.
	close(release)
	for {
		if v, ok := hostToolsCache.Load(stalled); ok && len(v.(hostToolsEntry).tools) > 0 {
			break // blocks until filled; `go test -timeout` is the clock
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, routes, _ := hostToolsNamed(context.Background(), servers); routes["stalled_tool"] != stalled {
		t.Fatalf("next launch routes = %v, want the late server's tool", routes)
	}
}
