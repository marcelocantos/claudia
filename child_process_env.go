// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ChildProcessEnv is a host-supplied environment overlay merged into each
// child process a sidecar seat spawns (Bash, ripgrep, and other tool
// children). Keys are variable names; values are passed verbatim. Empty or
// nil means no overlay — children still inherit the sidecar process baseline.
// Claudia does not interpret host-specific policy; hosts wire identity and
// guards externally (🎯T218).
type ChildProcessEnv = map[string]string

// ValidateChildProcessEnv rejects malformed overlay keys or values.
func ValidateChildProcessEnv(env ChildProcessEnv) error {
	if len(env) == 0 {
		return nil
	}
	for key, val := range env {
		if err := validateChildProcessEnvEntry(key, val); err != nil {
			return err
		}
	}
	return nil
}

func validateChildProcessEnvEntry(key, val string) error {
	if key == "" {
		return fmt.Errorf("child_process_env: empty variable name")
	}
	if strings.ContainsRune(key, '=') {
		return fmt.Errorf("child_process_env: variable name %q contains '='", key)
	}
	if strings.ContainsRune(key, 0) || strings.ContainsRune(val, 0) {
		return fmt.Errorf("child_process_env: variable %q contains NUL", key)
	}
	if !utf8.ValidString(key) || !utf8.ValidString(val) {
		return fmt.Errorf("child_process_env: variable %q is not valid UTF-8", key)
	}
	if !validChildProcessEnvName(key) {
		return fmt.Errorf("child_process_env: invalid variable name %q", key)
	}
	return nil
}

func validChildProcessEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// CloneChildProcessEnv returns a deep copy of env, or nil when empty.
func CloneChildProcessEnv(env ChildProcessEnv) ChildProcessEnv {
	if len(env) == 0 {
		return nil
	}
	out := make(ChildProcessEnv, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// normalizeChildProcessEnv validates and clones a non-empty overlay.
func normalizeChildProcessEnv(env ChildProcessEnv) (ChildProcessEnv, error) {
	if len(env) == 0 {
		return nil, nil
	}
	if err := ValidateChildProcessEnv(env); err != nil {
		return nil, err
	}
	return CloneChildProcessEnv(env), nil
}
