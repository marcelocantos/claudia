// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package tmuxagent

import "strings"

const (
	sessionWindowPrefix = "claudia-"
	poolWindowPrefix    = "claudia-pool-"
	sessionOptionKey    = "claudia-session-id"
)

// SessionWindowName is the tmux window name Start uses for a session.
// The first eight characters of the session id keep the name short;
// @claudia-session-id is the unambiguous match.
func SessionWindowName(sessionID string) string {
	if len(sessionID) > 8 {
		sessionID = sessionID[:8]
	}
	return sessionWindowPrefix + sessionID
}

// WindowsForSession returns every claudia session window that belongs
// to sessionID: either @claudia-session-id matches, or the window was
// named by SessionWindowName. Pool windows are never included.
func WindowsForSession(sessionID string) ([]string, error) {
	if sessionID == "" {
		return nil, nil
	}
	windows, err := ListWindows()
	if err != nil {
		return nil, err
	}
	wantName := SessionWindowName(sessionID)
	seen := make(map[string]bool)
	var ids []string
	for _, w := range windows {
		if strings.HasPrefix(w.Name, poolWindowPrefix) {
			continue
		}
		match := w.Name == wantName
		if sid, ok := GetWindowOption(w.ID, sessionOptionKey); ok && sid == sessionID {
			match = true
		}
		if match && !seen[w.ID] {
			seen[w.ID] = true
			ids = append(ids, w.ID)
		}
	}
	return ids, nil
}

// KillWindowsForSession kill-windows every window WindowsForSession
// reports. Missing windows are success. Used to reap leftovers before
// rematerialising a session, and by Registry.Stop when there is no
// in-memory handle.
func KillWindowsForSession(sessionID string) error {
	ids, err := WindowsForSession(sessionID)
	if err != nil {
		return err
	}
	var first error
	for _, id := range ids {
		if err := KillWindow(id); err != nil && first == nil {
			first = err
		}
	}
	return first
}
