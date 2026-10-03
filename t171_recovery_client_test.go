// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

type t171RecoveryHandler struct {
	calls       atomic.Int32
	unsupported bool
}

func (h *t171RecoveryHandler) ConnClosed(*broker.ClientConn) {}
func (h *t171RecoveryHandler) HandleRequest(c *broker.ClientConn, r *broker.Request) bool {
	h.calls.Add(1)
	if r.Type != broker.TypeAuthRecoverDetail {
		_ = c.Fail(r.ID, errors.New("unexpected fallback"))
		return true
	}
	if h.unsupported {
		_ = c.Fail(r.ID, &broker.ProtocolError{Code: broker.CodeUnknownType, Field: "type", Msg: "unsupported operation"})
		return true
	}
	_ = c.Reply(&broker.Response{ID: r.ID, Type: broker.TypeAuthRecoveryDetail, AuthDetail: &broker.AuthRecoveryDetailResponse{Provider: r.AuthDetail.Name, Outcome: "healthy_no_op", Classification: "none"}})
	return true
}
func TestT171RecoveryClientNeverFallsBack(t *testing.T) {
	for _, unsupported := range []bool{false, true} {
		t.Run(map[bool]string{false: "supported", true: "old-daemon"}[unsupported], func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "t171-client-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			sock := filepath.Join(dir, "b.sock")
			t.Setenv(broker.SocketPathEnv, sock)
			t.Setenv(broker.NoBrokerEnv, "")
			ln, err := broker.Listen(sock)
			if err != nil {
				t.Fatal(err)
			}
			h := &t171RecoveryHandler{unsupported: unsupported}
			srv, err := broker.Serve(&broker.ServeArgs{Listener: ln, Handler: h})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { srv.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := RecoverOMPAuthDetailed(ctx, "anthropic")
			if unsupported {
				var pe *broker.ProtocolError
				if !errors.As(err, &pe) || pe.Code != broker.CodeUnknownType {
					t.Fatalf("unsupported response: %v", err)
				}
			} else if err != nil || result.Outcome != "healthy_no_op" {
				t.Fatalf("%+v %v", result, err)
			}
			if h.calls.Load() != 1 {
				t.Fatal("client retried/fell back")
			}
		})
	}
}
