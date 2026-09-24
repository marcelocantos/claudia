// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

func TestOMPStartRefusesVendorCLI(t *testing.T) {
	t.Setenv("CLAUDIA_OMP_SOCKET", "")
	_, err := StartDirect(Config{
		Provider:    Provider(omp.Anthropic),
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err == nil || !strings.Contains(err.Error(), "vendor CLI") {
		t.Fatalf("err = %v", err)
	}
}

func TestOMPStartLoadsTokenFromKeychain(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-omp-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan omp.Message, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadBytes('\n')
		var msg omp.Message
		_ = json.Unmarshal(line, &msg)
		got <- msg
		_, _ = c.Write([]byte("{\"type\":\"ready\"}\n"))
	}()

	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"r","access_token":"plan-token","expiry":"` + exp + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(blob), nil
	}
	t.Cleanup(func() { ompKeychain = nil })
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)

	agent, err := StartDirect(Config{
		Name:        "seat",
		Provider:    Provider(omp.Anthropic),
		Model:       "claude-opus",
		WorkDir:     dir,
		TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	msg := <-got
	if msg.Op != omp.OpLoad || msg.Token != "plan-token" || msg.Provider != omp.Anthropic {
		t.Fatalf("load = %+v", msg)
	}
	if msg.Token == "" {
		t.Fatal("load carried no access token")
	}
}
