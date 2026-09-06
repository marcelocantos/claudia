// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"path/filepath"
)

// Exclusive configuration belongs to one conversation, not one process. Stop
// must never delete the provider's session store along with its MCP config.
func exclusiveGrokHomeDir(sessionID string) string {
	key := exclusiveCodexHomeKey(sessionID)
	if key == "" {
		return ""
	}
	return filepath.Join(claudiaStateHome(), "grok-homes", key)
}

func exclusiveGrokHomeForStart(sessionID string, requireResume bool) (string, error) {
	home := exclusiveGrokHomeDir(sessionID)
	if home == "" && sessionID != "" {
		return "", fmt.Errorf("exclusive GROK_HOME: invalid session identity")
	}
	if home != "" {
		info, err := os.Stat(home)
		if err == nil && !info.IsDir() {
			return "", fmt.Errorf("exclusive GROK_HOME is not a directory: %s", home)
		}
		if err != nil && (!os.IsNotExist(err) || requireResume) {
			return "", fmt.Errorf("exclusive GROK_HOME unavailable for session %s: %w", sessionID, err)
		}
		if err := os.MkdirAll(home, 0o700); err != nil {
			return "", err
		}
	} else {
		if requireResume {
			return "", fmt.Errorf("exclusive GROK_HOME resume requires a session identity")
		}
		root := filepath.Join(claudiaStateHome(), "grok-homes")
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		var err error
		home, err = os.MkdirTemp(root, "pending-")
		if err != nil {
			return "", err
		}
	}
	if err := writeExclusiveGrokHome(home); err != nil {
		return "", err
	}
	return home, nil
}

// session/new may replace the requested ID. Publish its actual ID before Start
// returns, without moving the directory the running provider still writes to.
func publishExclusiveGrokHome(home, sessionID string) error {
	dest := exclusiveGrokHomeDir(sessionID)
	if dest == "" {
		return fmt.Errorf("publish exclusive GROK_HOME: invalid session identity")
	}
	source, err := filepath.EvalSymlinks(home)
	if err != nil {
		return err
	}
	if existing, err := filepath.EvalSymlinks(dest); err == nil {
		if existing != source {
			return fmt.Errorf("exclusive GROK_HOME identity collision for %s", sessionID)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := os.Symlink(source, dest); err != nil {
		// Another publisher may have installed the identical mapping. A
		// different mapping (or dangling link) remains an explicit error.
		existing, resolveErr := filepath.EvalSymlinks(dest)
		if resolveErr != nil || existing != source {
			return fmt.Errorf("publish exclusive GROK_HOME: %w", err)
		}
	}
	return nil
}
