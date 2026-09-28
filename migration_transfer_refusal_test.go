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

// 🎯T137: a transfer seat whose destination refuses the turn (usage limit,
// rate limit, auth) ends with no text. SummarizeForMigration must name the
// refusal; "empty brief" is only for a turn that completed with no text.
// Observed in Jevons J34: Codex at 0% weekly room failed as
// "migration transfer: empty brief".
func TestT137TransferNamesDestinationRefusal(t *testing.T) {
	const reason = "429 usage_limit_reached: weekly limit resets 2026-10-03T23:20Z"
	for _, tc := range []struct {
		name    string
		reply   []string // sidecar lines answering the prompt
		brief   string
		wantErr string
		notErr  string
	}{
		{
			name:    "refusal",
			reply:   []string{`{"type":"turn_end","error":"` + reason + `","snapshot":{"messages":[]}}`},
			wantErr: "provider refused the turn: " + reason,
			notErr:  "empty brief",
		},
		{
			// A sidecar started before turn_end carried error: the reason
			// is only in the snapshot's last message.
			name: "refusal from an older sidecar",
			reply: []string{`{"type":"turn_end","snapshot":{"messages":[{"role":"user","content":"x"},` +
				`{"role":"assistant","content":[{"type":"text","text":""}],"stopReason":"error","errorMessage":"` + reason + `"}]}}`},
			wantErr: "provider refused the turn: " + reason,
			notErr:  "empty brief",
		},
		{
			name:    "completed with no text",
			reply:   []string{`{"type":"turn_end","snapshot":{"messages":[{"role":"assistant","content":[],"stopReason":"stop"}]}}`},
			wantErr: "migration transfer: empty brief",
		},
		{
			name: "aborted is not a refusal",
			reply: []string{`{"type":"turn_end","snapshot":{"messages":[` +
				`{"role":"assistant","content":[],"stopReason":"aborted","errorMessage":"Request was aborted"}]}}`},
			wantErr: "migration transfer: empty brief",
		},
		{
			name:  "answered",
			reply: []string{`{"type":"text","text":"Continue T691; VIOLET67 retained"}`, `{"type":"turn_end","snapshot":{"messages":[]}}`},
			brief: "Continue T691; VIOLET67 retained",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startFakeTransferSidecar(t, tc.reply)
			res, err := SummarizeForMigration(context.Background(), MigrationTransferArgs{
				Destination: ProviderClaude, Goal: "T691", Transcript: "user: finish T691\n",
			})
			if tc.wantErr == "" {
				if err != nil || res.Brief != tc.brief {
					t.Fatalf("SummarizeForMigration = %+v, %v; want brief %q", res, err, tc.brief)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("SummarizeForMigration err = %v; want it to contain %q", err, tc.wantErr)
			}
			if tc.notErr != "" && strings.Contains(err.Error(), tc.notErr) {
				t.Fatalf("refusal reported as %q: %v", tc.notErr, err)
			}
		})
	}
}

// startFakeTransferSidecar serves one sidecar connection that loads a seat
// and answers its prompt with reply, and points the transfer at a direct
// start on it with a plan token in a fake Keychain.
func startFakeTransferSidecar(t *testing.T, reply []string) {
	t.Helper()
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("claudia-t137-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	t.Cleanup(func() { os.Remove(socket) })
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var msg omp.Message
			_ = json.Unmarshal(line, &msg)
			switch msg.Op {
			case omp.OpLoad:
				_, _ = c.Write([]byte("{\"type\":\"ready\"}\n"))
			case omp.OpPrompt:
				for _, l := range reply {
					_, _ = c.Write([]byte(l + "\n"))
				}
			}
		}
	}()
	t.Setenv("CLAUDIA_OMP_SOCKET", socket)

	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"r","access_token":"plan-token","expiry":"` + exp + `"}}}`
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) { return []byte(blob), nil }
	t.Cleanup(func() { ompKeychain = nil; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}

	prev := migrationTransferStart
	migrationTransferStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		cfg.TermLogPath = "-"
		return startDirectContext(ctx, cfg)
	}
	t.Cleanup(func() { migrationTransferStart = prev })
}
