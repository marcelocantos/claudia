// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestResolveCodexBin(t *testing.T) {
	fakeCodex := "/tmp/fake-codex"
	fakeAppCodex := "/Applications/Codex.app/Contents/Resources/codex"
	errNotFound := errors.New("not found")

	statExisting := func(paths ...string) func(string) (os.FileInfo, error) {
		exists := make(map[string]bool, len(paths))
		for _, p := range paths {
			exists[p] = true
		}
		return func(path string) (os.FileInfo, error) {
			if exists[path] {
				return nil, nil
			}
			return nil, errNotFound
		}
	}

	t.Run("CODEX_BIN absolute path that exists is honoured", func(t *testing.T) {
		got, err := resolveCodexBinFrom(
			func(string) string { return fakeCodex },
			func(string) (string, error) { return "", errNotFound },
			statExisting(fakeCodex),
			nil,
		)
		if err != nil {
			t.Fatalf("resolveCodexBinFrom: %v", err)
		}
		if got != fakeCodex {
			t.Errorf("got %q, want %q", got, fakeCodex)
		}
	})

	t.Run("CODEX_BIN relative name is resolved via PATH", func(t *testing.T) {
		got, err := resolveCodexBinFrom(
			func(string) string { return codexBinName },
			func(name string) (string, error) {
				if name == codexBinName {
					return fakeCodex, nil
				}
				return "", errNotFound
			},
			statExisting(),
			nil,
		)
		if err != nil {
			t.Fatalf("resolveCodexBinFrom: %v", err)
		}
		if got != fakeCodex {
			t.Errorf("got %q, want %q", got, fakeCodex)
		}
	})

	t.Run("PATH lookup wins when CODEX_BIN is unset", func(t *testing.T) {
		got, err := resolveCodexBinFrom(
			func(string) string { return "" },
			func(name string) (string, error) {
				if name == codexBinName {
					return fakeCodex, nil
				}
				return "", errNotFound
			},
			statExisting(),
			nil,
		)
		if err != nil {
			t.Fatalf("resolveCodexBinFrom: %v", err)
		}
		if got != fakeCodex {
			t.Errorf("got %q, want %q", got, fakeCodex)
		}
	})

	t.Run("app bundle candidate is checked after PATH miss", func(t *testing.T) {
		got, err := resolveCodexBinFrom(
			func(string) string { return "" },
			func(string) (string, error) { return "", errNotFound },
			statExisting(fakeAppCodex),
			[]string{fakeAppCodex},
		)
		if err != nil {
			t.Fatalf("resolveCodexBinFrom: %v", err)
		}
		if got != fakeAppCodex {
			t.Errorf("got %q, want %q", got, fakeAppCodex)
		}
	})

	t.Run("missing everywhere returns error mentioning CODEX_BIN", func(t *testing.T) {
		_, err := resolveCodexBinFrom(
			func(string) string { return "" },
			func(string) (string, error) { return "", errNotFound },
			statExisting(),
			[]string{fakeAppCodex},
		)
		if err == nil {
			t.Fatal("expected error when codex is absent")
		}
		if !strings.Contains(err.Error(), codexBinEnv) {
			t.Errorf("error %q does not mention %s", err.Error(), codexBinEnv)
		}
	})
}

func TestCodexBinCandidatesIncludeDesktopAppBundle(t *testing.T) {
	const appBundleCodex = "/Applications/Codex.app/Contents/Resources/codex"
	for _, candidate := range codexBinCandidates() {
		if candidate == appBundleCodex {
			return
		}
	}
	t.Fatalf("codexBinCandidates() does not include %s", appBundleCodex)
}

func TestCodexAppServerFixturesAreValidJSONL(t *testing.T) {
	cases := []struct {
		path       string
		wantToken  string
		wantMethod string
	}{
		{"testdata/codex/app-server/thread-start.jsonl", "thr_redacted", "thread/started"},
		{"testdata/codex/app-server/success.jsonl", "turn_success", "turn/completed"},
		{"testdata/codex/app-server/failure.jsonl", "model failed", "turn/completed"},
		{"testdata/codex/app-server/interrupted.jsonl", "turn_interrupted", "turn/completed"},
		{"testdata/codex/app-server/unsupported-capability.jsonl", "experimentalApi", ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var sawMethod bool
			var sawToken bool
			for _, line := range readFixtureLines(t, tc.path) {
				var msg map[string]any
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					t.Fatalf("invalid JSONL line %q: %v", line, err)
				}
				if method, _ := msg["method"].(string); method == tc.wantMethod {
					sawMethod = true
				}
				if strings.Contains(line, tc.wantToken) {
					sawToken = true
				}
			}
			if tc.wantMethod != "" && !sawMethod {
				t.Errorf("%s did not contain method %s", tc.path, tc.wantMethod)
			}
			if !sawToken {
				t.Errorf("%s did not contain token %s", tc.path, tc.wantToken)
			}
		})
	}
}
