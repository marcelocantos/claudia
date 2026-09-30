// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

// 🎯T163: a Grok or Cursor seat starts through the Oh My Pi sidecar. When
// the sidecar refuses the seat's load, the broker's caller gets the
// sidecar's own words and what to do about them — restart the broker
// service, and the PATH a supervised broker needs — rather than the bare
// `omp: sidecar said "error", want ready`. Origin PR #59 gave those hints
// on the vendor CLI path, which this line no longer takes.

// t163SidecarWords is what the fake sidecar answers a load with: a Bun
// process that could not find a command on the PATH it inherited.
const t163SidecarWords = "spawn bash ENOENT"

// fakeRefusingSidecar listens on a sidecar socket and answers every load
// with an error event carrying t163SidecarWords.
func fakeRefusingSidecar(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "t163")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "omp.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var msg omp.Message
					if json.Unmarshal(line, &msg) != nil {
						return
					}
					if msg.Op == omp.OpLoad {
						out, _ := json.Marshal(map[string]string{"seat": msg.Seat, "type": "error", "text": t163SidecarWords})
						_, _ = conn.Write(append(out, '\n'))
					}
				}
			}()
		}
	}()
	return socket
}

// seedPlans gives this process a live login for every subscription plan,
// through the same one-shot read the broker makes at startup, so a grant
// reaches the sidecar instead of stopping at a login.
func seedPlans(t *testing.T) {
	t.Helper()
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec := `{"refresh_token":"r","access_token":"token","expiry":"` + exp + `"}`
	blob := `{"records":{"xai-oauth":` + rec + `,"cursor":` + rec + `}}`
	store := omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) { return []byte(blob), nil }}
	if err := omp.Open(t.Context(), store); err != nil {
		t.Fatal(err)
	}
}

func TestT163SidecarRefusalSaysWhatToDo(t *testing.T) {
	for _, provider := range []claudia.Provider{claudia.ProviderGrok, claudia.ProviderCursor} {
		t.Run(string(provider), func(t *testing.T) {
			seedPlans(t)
			socket := fakeRefusingSidecar(t)
			t.Setenv(omp.SocketEnv, socket)

			f := newFixture(t)
			opts := f.options(nil)
			opts.launchers = nil // the real start: the sidecar path
			opts.DisableResume = true
			opts.DisableMCPHost = true
			opts.DisableIntel = true
			f.bootWith(t, opts)

			_, err := claudia.Start(claudia.Config{
				Name:        "t163-" + string(provider),
				Provider:    provider,
				WorkDir:     t.TempDir(),
				TermLogPath: "-",
			})
			if err == nil {
				t.Fatal("broker Start succeeded; want the sidecar's refusal")
			}
			msg := err.Error()
			t.Log(msg)
			for _, want := range []string{
				"agent_failed",
				`sidecar said "error", want ready`,
				t163SidecarWords, // the sidecar's own words
				// restart the broker service
				"supervisorctl restart claudia",
				"brew services restart claudia",
				// the PATH a supervised broker needs
				"PATH",
				"~/.bun/bin",
				"~/.grok/bin",
				"supervisor/claudia.ini",
				// where to look next
				socket + ".pid",
				filepath.Join(filepath.Dir(socket), "omp-sidecar.log"),
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("error missing %q:\n%s", want, msg)
				}
			}
		})
	}
}

// TestT163SidecarRefusalNamesWhatTheServicePATHLacks is the same refusal
// from a broker on a launchd-style service PATH: the explanation names
// what that PATH is missing on this machine, not only what it should hold.
func TestT163SidecarRefusalNamesWhatTheServicePATHLacks(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "t163home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	bunDir := filepath.Join(home, ".bun", "bin")
	if err := os.MkdirAll(bunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	seedPlans(t)
	t.Setenv(omp.SocketEnv, fakeRefusingSidecar(t))
	f := newFixture(t)
	opts := f.options(nil)
	opts.launchers = nil
	opts.DisableResume = true
	opts.DisableMCPHost = true
	opts.DisableIntel = true
	f.bootWith(t, opts)

	_, err = claudia.Start(claudia.Config{
		Name:        "t163-service-path",
		Provider:    claudia.ProviderGrok,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err == nil {
		t.Fatal("broker Start succeeded; want the sidecar's refusal")
	}
	msg := err.Error()
	for _, want := range []string{
		t163SidecarWords,
		"This broker's PATH has no bun.",
		"Present on this machine but not on its PATH: " + bunDir,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q:\n%s", want, msg)
		}
	}
}
