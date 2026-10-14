// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import "strings"

// claudeSessionMarkers are the variables Claude Code sets on the processes
// it runs to say "you are inside my session": its identity, its IPC channel,
// and the child-session flag. A seat claudia spawns is a session of its own,
// so none of them may reach it (🎯T121).
//
// Inherited, they change the seat's behaviour. A seat started by a host that
// was itself running inside Claude Code printed "Transcript saving is off —
// inherited CLAUDE_CODE_CHILD_SESSION marker" and wrote no transcript, and
// claudia reads a Claude seat's turns from that transcript: the turn was
// answered on screen while WaitForResponse saw nothing but progress events
// for two minutes (gate 8bfc1eb3, 2026-09-22).
//
// This is a list of session markers, not a CLAUDE_* wildcard. Variables that
// configure Claude Code (CLAUDE_EFFORT, CLAUDE_CODE_USE_BEDROCK, …) are the
// operator's settings and must still reach the seat.
var claudeSessionMarkers = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_EXECPATH",
	"CLAUDE_PID",
}

// stripClaudeSessionMarkers returns env (in os.Environ() "K=V" form) without
// any of claudeSessionMarkers.
func stripClaudeSessionMarkers(env []string) []string {
	drop := make(map[string]bool, len(claudeSessionMarkers))
	for _, name := range claudeSessionMarkers {
		drop[name] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if name, _, ok := strings.Cut(kv, "="); ok && drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
