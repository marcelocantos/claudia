// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDisabledHonoursNoBrokerEnv(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want bool
	}{
		{name: "unset", want: false},
		{name: "empty", set: true, val: "", want: false},
		{name: "zero", set: true, val: "0", want: false},
		{name: "false", set: true, val: "false", want: false},
		{name: "no", set: true, val: "no", want: false},
		{name: "off", set: true, val: "off", want: false},
		{name: "one", set: true, val: "1", want: true},
		{name: "true", set: true, val: "true", want: true},
		{name: "yes", set: true, val: "yes", want: true},
		{name: "TRUE", set: true, val: "TRUE", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(NoBrokerEnv, "sentinel")
			if tc.set {
				t.Setenv(NoBrokerEnv, tc.val)
			} else if err := os.Unsetenv(NoBrokerEnv); err != nil {
				t.Fatal(err)
			}
			if got := Disabled(); got != tc.want {
				t.Errorf("Disabled() with %s=%q (set=%v) = %v, want %v",
					NoBrokerEnv, tc.val, tc.set, got, tc.want)
			}
		})
	}
}

func TestSocketPathDefaultIsWellKnown(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv(SocketPathEnv, "")
	t.Setenv(StateHomeEnv, "")
	t.Setenv("HOME", home)
	got, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join(home, xdgLocalDir, xdgStateDir, stateSubdir, socketName))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("SocketPath() = %q, want %q", got, want)
	}
}

func TestSocketPathHonoursXDGStateHome(t *testing.T) {
	t.Setenv(SocketPathEnv, "")
	t.Setenv(StateHomeEnv, "/var/state")
	got, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join("/var/state", stateSubdir, socketName))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("SocketPath() = %q, want %q", got, want)
	}
}

func TestSocketPathHonoursOverride(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "co")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	override := filepath.Join(dir, "c.sock")
	t.Setenv(SocketPathEnv, override)
	got, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(override)
	if err != nil {
		t.Fatal(err)
	}
	if got != abs {
		t.Errorf("SocketPath() = %q, want %q", got, abs)
	}
}
