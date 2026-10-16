// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestTaskCLICommandRefusesWithoutBroker(t *testing.T) {
	t.Setenv(broker.SocketPathEnv, filepath.Join(t.TempDir(), "absent.sock"))
	for _, provider := range []string{"", "claude", "codex", "grok", "cursor"} {
		args := []string{"--provider", provider, "--workdir", t.TempDir(), "hello"}
		if err := taskCmd(args); !errors.Is(err, claudia.ErrTaskCLIRemoved) {
			t.Fatalf("provider %q: %v", provider, err)
		}
	}
	if err := taskCmd([]string{"--pick", "remaining", "hello"}); !errors.Is(err, claudia.ErrTaskCLIRemoved) {
		t.Fatal(err)
	}
	err := taskWireError(&broker.ProtocolError{Code: broker.CodeTaskCLIRemoved, Msg: "removed"})
	if !errors.Is(err, claudia.ErrTaskCLIRemoved) {
		t.Fatalf("wire lost sentinel: %v", err)
	}
}
