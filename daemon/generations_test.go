// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

func TestGenerationDiscoveryActualHistoryDirectAndBroker(t *testing.T) {
	f := newFixture(t)
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(f.sock)
	socket := filepath.Join(root, "omp.sock")
	store := filepath.Join(root, "omp-seats", "removed")
	if err := os.MkdirAll(store, 0700); err != nil {
		t.Fatal(err)
	}
	// Neither registry nor spool contains this removed seat. A large conversation
	// tail is immaterial; discovery reads only bounded headers.
	for _, session := range []string{"before-remint", "after-remint"} {
		h := fmt.Sprintf("{\"type\":\"claudia_seat\",\"v\":1,\"seat\":\"removed\",\"session\":%q}\n", session)
		if err := os.WriteFile(filepath.Join(store, session+".jsonl"), []byte(h+string(make([]byte, 1<<20))), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bad := filepath.Join(store, "corrupt.jsonl")
	if err := os.WriteFile(bad, []byte("{torn"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bun, "../sidecar/server.ts", socket)
	cmd.Env = append(os.Environ(), "JEVONS_SPOOL_DIR="+filepath.Join(root, "spool"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	t.Setenv(omp.SocketEnv, socket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !omp.Listening(ctx, socket) {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	f.boot(t, nil)
	for _, query := range []func(context.Context, claudia.GenerationQuery) (*claudia.GenerationPage, error){claudia.DiscoverGenerationsDirect, claudia.DiscoverGenerations} {
		cursor := ""
		var ids []string
		issues := 0
		for pages := 0; ; pages++ {
			if pages > 16 {
				t.Fatal("discovery did not terminate")
			}
			page, err := query(ctx, claudia.GenerationQuery{Cursor: cursor, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range page.Generations {
				ids = append(ids, g.Seat+"/"+g.SessionID)
			}
			issues += len(page.Issues)
			if cursor != "" {
				retry, err := query(ctx, claudia.GenerationQuery{Cursor: cursor, Limit: 1})
				if err != nil || !reflect.DeepEqual(page, retry) {
					t.Fatalf("retry lost page: %+v %v", retry, err)
				}
			}
			if !page.More {
				if page.Complete {
					t.Fatal("corrupt inventory claimed complete")
				}
				break
			}
			cursor = page.Cursor
		}
		sort.Strings(ids)
		if !reflect.DeepEqual(ids, []string{"removed/after-remint", "removed/before-remint"}) || issues != 1 {
			t.Fatalf("inventory=%v issues=%d", ids, issues)
		}
	}
	raw, err := os.ReadFile(bad)
	if err != nil || string(raw) != "{torn" {
		t.Fatalf("discovery repaired history: %q %v", raw, err)
	}
	if f.seatCount() != 0 {
		t.Fatal("discovery loaded a provider")
	}
	_, err = claudia.DiscoverGenerations(ctx, claudia.GenerationQuery{Cursor: "expired:1", Limit: 1})
	var de *claudia.GenerationDiscoveryError
	if !errors.As(err, &de) || de.Code != claudia.DiscoveryCursorExpired {
		t.Fatalf("expiry lost typed identity: %T %v", err, err)
	}
}

func TestGenerationDiscoveryBrokerPreservesDeadlineAndNoCredentials(t *testing.T) {
	f := newFixture(t)
	socket := filepath.Join(filepath.Dir(f.sock), "generations.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv(omp.SocketEnv, socket)
	seen := make(chan omp.Message, 2)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var msg omp.Message
			if json.NewDecoder(conn).Decode(&msg) == nil {
				seen <- msg
				_ = json.NewEncoder(conn).Encode(omp.Event{Type: "generation_page", RPCID: msg.RPCID, GenerationPage: &omp.GenerationPage{Generations: []omp.GenerationIdentity{}, Issues: []omp.GenerationIssue{}, Complete: true}})
			}
			_ = conn.Close()
		}
	}()
	f.boot(t, nil)
	for _, query := range []func(context.Context, claudia.GenerationQuery) (*claudia.GenerationPage, error){claudia.DiscoverGenerationsDirect, claudia.DiscoverGenerations} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		deadline, _ := ctx.Deadline()
		page, err := query(ctx, claudia.GenerationQuery{Cursor: "opaque:1", Limit: 1})
		cancel()
		if err != nil || !page.Complete {
			t.Fatalf("discovery=%+v %v", page, err)
		}
		msg := <-seen
		if msg.ExpiresAt != deadline.UnixMilli() || msg.Cursor != "opaque:1" || msg.Limit != 1 || msg.Op != "generations" || msg.Token != "" || msg.Provider != "" || msg.Seat != "" || msg.SessionID != "" {
			t.Fatalf("bad discovery wire %+v", msg)
		}
	}
	if f.seatCount() != 0 {
		t.Fatal("discovery loaded a provider")
	}
}
