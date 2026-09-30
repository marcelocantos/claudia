// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
)

func TestCLIVersionFlags(t *testing.T) {
	for _, flag := range []string{"version", "--version", "-v"} {
		out := captureStdout(t, func() error {
			if code := run([]string{flag}); code != 0 {
				t.Fatalf("%s: exit %d", flag, code)
			}
			return nil
		})
		if !strings.Contains(out, claudia.Version) {
			t.Fatalf("%s: want version %q, got %q", flag, claudia.Version, out)
		}
	}
}

func TestCLIHelp(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "help"} {
		errOut := captureStderr(t, func() error {
			if code := run([]string{flag}); code != 0 {
				t.Fatalf("%s: exit %d", flag, code)
			}
			return nil
		})
		for _, want := range []string{"usage: claudia broker", "usage|task|release", "grant|send|interrupt|events", "--version", "--help-agent"} {
			if !strings.Contains(errOut, want) {
				t.Fatalf("%s missing %q:\n%s", flag, want, errOut)
			}
		}
	}
}

func TestCLIHelpAgent(t *testing.T) {
	out := captureStdout(t, func() error {
		if code := run([]string{"--help-agent"}); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return nil
	})
	for _, want := range []string{
		"usage: claudia broker",
		"claudia models intel",
		"Daemon: `claudia broker`",
		"CLAUDIA_NO_BROKER=1",
		"client-side fold",
		"pimp-smoke",
		"~/.local/state/claudia/broker.sock",
		"0.44.0",
		"Task one-shot over the broker",
		"RunBrokerTask",
		"ADMIT",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("--help-agent missing %q:\n%s", want, out)
		}
	}
}
