// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
)

// findCodexRollout returns a rollout path for thread id under exclusive
// CODEX_HOME, or "". Live Codex writes sessions/YYYY/MM/DD/rollout-*-<id>.jsonl;
// the hermetic fake writes sessions/<id>/rollout.json.
func findCodexRollout(home, threadID string) string {
	home = strings.TrimSpace(home)
	threadID = strings.TrimSpace(threadID)
	if home == "" || threadID == "" {
		return ""
	}
	legacy := filepath.Join(home, "sessions", threadID, "rollout.json")
	if st, err := os.Stat(legacy); err == nil && !st.IsDir() && st.Size() > 0 {
		return legacy
	}
	root := filepath.Join(home, "sessions")
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.Contains(name, threadID) {
			return nil
		}
		if !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".json") {
			return nil
		}
		if st, err := d.Info(); err == nil && st.Size() > 0 {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):]
		}
	}
	return ""
}
