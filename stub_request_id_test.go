// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"testing"
)

type stubRequest struct {
	text string
	id   string
}

// TestStubSendWithRequestIDReachesHook drives Agent.SendWithRequestID.
// A hook on either stub constructor sees the id. A nil hook still calls Send.
func TestStubSendWithRequestIDReachesHook(t *testing.T) {
	t.Run("NewStubAgentOps", func(t *testing.T) {
		var plain []string
		var tagged []stubRequest
		a := NewStubAgentOps(&StubAgentOps{
			Send: func(text string) error {
				plain = append(plain, text)
				return fmt.Errorf("untagged Send")
			},
			SendWithRequestID: func(text, id string) error {
				tagged = append(tagged, stubRequest{text: text, id: id})
				return nil
			},
		})
		if err := a.SendWithRequestID("fleet note", "fn-1"); err != nil {
			t.Fatal(err)
		}
		if len(plain) != 0 || len(tagged) != 1 || tagged[0] != (stubRequest{text: "fleet note", id: "fn-1"}) {
			t.Fatalf("plain=%v tagged=%v", plain, tagged)
		}
	})

	t.Run("nil hook keeps Send", func(t *testing.T) {
		var plain []string
		a := NewStubAgentOps(&StubAgentOps{
			Send: func(text string) error {
				plain = append(plain, text)
				return nil
			},
		})
		if err := a.SendWithRequestID("plain", "fn-dropped"); err != nil {
			t.Fatal(err)
		}
		if len(plain) != 1 || plain[0] != "plain" {
			t.Fatalf("Send = %v", plain)
		}
	})

	t.Run("StartStub", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		var plain []string
		var tagged []stubRequest
		a, err := StartStub(context.Background(), Config{WorkDir: dir, SessionID: "sid-req", TermLogPath: "-"}, &StubAgentOps{
			Send: func(text string) error {
				plain = append(plain, text)
				return fmt.Errorf("untagged Send")
			},
			SendWithRequestID: func(text, id string) error {
				tagged = append(tagged, stubRequest{text: text, id: id})
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Stop)
		if err := a.SendWithRequestID("owner note", "om-1"); err != nil {
			t.Fatal(err)
		}
		if len(plain) != 0 || len(tagged) != 1 || tagged[0] != (stubRequest{text: "owner note", id: "om-1"}) {
			t.Fatalf("plain=%v tagged=%v", plain, tagged)
		}
	})
}
