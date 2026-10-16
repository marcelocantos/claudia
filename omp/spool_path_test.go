// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Drive the Bun default writer and Go reader against the same environment,
// including legacy consumer settings that must have no authority here.
func TestSpoolDefaultsMatchBunWriter(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	legacy := filepath.Join(root, "legacy-spool")
	for _, tc := range []struct {
		name, state, override, want string
	}{
		{"home", "", "", filepath.Join(home, ".local", "state", "claudia", "spool")},
		{"xdg", " " + filepath.Join(root, "xdg") + " ", "", filepath.Join(root, "xdg", "claudia", "spool")},
		{"relative xdg ignored", "relative-state", "", filepath.Join(home, ".local", "state", "claudia", "spool")},
		{"blank override", "", " \t ", filepath.Join(home, ".local", "state", "claudia", "spool")},
		{"explicit", filepath.Join(root, "unused"), " " + filepath.Join(root, "explicit") + " ", filepath.Join(root, "explicit")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", tc.state)
			t.Setenv("CLAUDIA_OMP_SPOOL_DIR", tc.override)
			t.Setenv("JEVONS_SPOOL_DIR", legacy)
			if got := SpoolDir(); got != tc.want {
				t.Fatalf("Go path=%q, want %q", got, tc.want)
			}
			cmd := exec.CommandContext(t.Context(), "bun", "--eval", `
import {spoolDir, defaultWriter} from "./spool.ts";
const writer = defaultWriter();
writer.append({ts:"2026-10-16T00:00:00Z", seat:"fixture", type:"text", text:"preserved"});
process.stdout.write(JSON.stringify({path:spoolDir(), writer:writer.dir}));`)
			cmd.Dir = filepath.Join("..", "sidecar")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Bun writer: %v: %s", err, out)
			}
			var got struct{ Path, Writer string }
			if err := json.Unmarshal(out, &got); err != nil || got.Path != tc.want || got.Writer != tc.want {
				t.Fatalf("Bun path=%s: %v, want %q", out, err, tc.want)
			}
			records, err := ReadSeat(SpoolDir(), "fixture")
			if err != nil || len(records) == 0 || records[len(records)-1].Text != "preserved" {
				t.Fatalf("Go reader missed Bun output: %+v, %v", records, err)
			}
		})
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy consumer spool was touched: %v", err)
	}
}
