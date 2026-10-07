// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// The fixture sidecar behaves like a seat that bound only its coding tools:
// it receives the nonempty host offer but reports only the tools actually
// bound. A ready event by itself must not allow the broker to launch it.
func TestT173RefusesReadyWithUnboundHostTools(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"initialize"`):
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`)
		case strings.Contains(string(body), `"tools/list"`):
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"jevons_agent_send","inputSchema":{"type":"object"}},{"name":"jevons_agent_list","inputSchema":{"type":"object"}}]}}`)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer fixture.Close()

	dir, err := os.MkdirTemp("/tmp", "t173-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "sidecar.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	loads := make(chan omp.Message, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var msg omp.Message
				if json.Unmarshal(line, &msg) != nil {
					return
				}
				loads <- msg
				// Bash is bound, but neither host-offered jevons_* tool is.
				how := "launched"
				if msg.Op == omp.OpAdopt {
					how = "adopted"
				}
				_, _ = io.WriteString(c, `{"type":"ready","how":"`+how+`","bound_tools":["Bash"]}`+"\n")
			}()
		}
	}()
	t.Setenv(omp.SocketEnv, socket)
	var refreshes atomic.Int32
	t141Plan(t, "token", &refreshes, "")
	for _, adopt := range []bool{false, true} {
		t.Run(map[bool]string{false: "load", true: "adopt"}[adopt], func(t *testing.T) {
			agent, err := StartDirect(Config{
				Name: "t173-seat", Provider: Provider(omp.Anthropic), Model: "claude-sonnet",
				WorkDir: t.TempDir(), TermLogPath: "-", AdoptOnly: adopt,
				MCPServers: []MCPServer{{Name: "jevons", Type: "http", URL: fixture.URL}},
			})
			if agent != nil {
				agent.Stop()
				t.Fatal("unbound tools must not launch an agent")
			}
			load := <-loads
			wantOp := omp.OpLoad
			if adopt {
				wantOp = omp.OpAdopt
			}
			var offered []hostJevonsTool
			if e := json.Unmarshal(load.Tools, &offered); e != nil || len(offered) != 2 || load.Op != wantOp {
				t.Fatalf("sidecar %s offered %s: %v, want both host tools on %s", load.Op, load.Tools, e, wantOp)
			}
			if err == nil || !strings.Contains(err.Error(), `t173-seat`) ||
				!strings.Contains(err.Error(), "jevons_agent_send") || !strings.Contains(err.Error(), "jevons_agent_list") {
				t.Fatalf("launch error = %v, want seat and both unbound names", err)
			}
		})
	}
}
