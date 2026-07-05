// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RewindResult reports what a rewind removed from a session transcript.
type RewindResult struct {
	// SessionID is the session that was rewound (empty when rewinding a raw
	// path via the lower-level entry point).
	SessionID string
	// JSONLPath is the transcript file that was truncated.
	JSONLPath string
	// TurnsRemoved is the number of user turns rolled back.
	TurnsRemoved int
	// LinesRemoved is the number of JSONL lines removed.
	LinesRemoved int
	// BytesRemoved is the number of bytes truncated from the transcript.
	BytesRemoved int64
	// BackupPath holds a complete copy of the pre-rewind transcript, so the
	// rewind is undoable with [Unrewind].
	BackupPath string
}

// RewindSession rolls the transcript for sessionID (under workDir) back by n
// user turns and truncates it at that turn boundary. The next Start or Run with
// this session id resumes from the rewound state — claude --resume replays only
// the surviving prefix (verified: a truncated transcript is honoured, not
// restored from elsewhere).
//
// A user turn is a genuine human prompt; tool-result entries (which the
// transcript also records with role "user") are not counted, so a rewind never
// lands mid-tool-use. The full pre-rewind transcript is copied to a backup
// sidecar first, making the rewind undoable with [Unrewind].
//
// The caller must stop any live process on this session before rewinding: a
// running claude holds the conversation in memory and would re-append the
// dropped turns when it exits. [Agent.Rewind] handles that for Session-mode
// agents.
func RewindSession(sessionID, workDir string, n int) (*RewindResult, error) {
	// claude derives the project directory from the resolved cwd, so resolve
	// symlinks (e.g. macOS /var -> /private/var) to match where it writes.
	resolved, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		resolved = workDir
	}
	res, err := rewindJSONL(SessionJSONLPath(sessionID, resolved), n)
	if res != nil {
		res.SessionID = sessionID
	}
	return res, err
}

// Unrewind restores the transcript saved by the most recent rewind of path,
// undoing the rewind (and discarding any turns added since). It removes the
// backup sidecar on success. Returns an error if no backup exists.
func Unrewind(path string) error {
	backup := path + rewindBackupSuffix
	data, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("unrewind: read backup: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("unrewind: restore transcript: %w", err)
	}
	return os.Remove(backup)
}

const rewindBackupSuffix = ".rewind-bak"

// rewindJSONL is the transcript-level core of rewinding: it truncates the
// JSONL at path back by n user turns and copies the original to a backup
// sidecar. It is pure file manipulation with no session or process knowledge,
// which is what makes it deterministically testable.
func rewindJSONL(path string, n int) (*RewindResult, error) {
	if n < 1 {
		return nil, fmt.Errorf("rewind: n must be >= 1, got %d", n)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rewind: read transcript: %w", err)
	}

	// Byte offset of the start of each genuine user-prompt line.
	var userOffsets []int64
	lineStart := 0
	for i := 0; i <= len(data); i++ {
		if i == len(data) || data[i] == '\n' {
			line := data[lineStart:i]
			if len(line) > 0 && isUserPromptLine(line) {
				userOffsets = append(userOffsets, int64(lineStart))
			}
			lineStart = i + 1
		}
	}

	turns := len(userOffsets)
	if n > turns {
		return nil, fmt.Errorf("rewind: cannot roll back %d turn(s); transcript has %d user turn(s)", n, turns)
	}

	cut := userOffsets[turns-n]
	removed := data[cut:]

	backup := path + rewindBackupSuffix
	if err := os.WriteFile(backup, data, 0o644); err != nil {
		return nil, fmt.Errorf("rewind: write backup: %w", err)
	}
	if err := os.Truncate(path, cut); err != nil {
		return nil, fmt.Errorf("rewind: truncate transcript: %w", err)
	}

	linesRemoved := 0
	for _, b := range removed {
		if b == '\n' {
			linesRemoved++
		}
	}
	if len(removed) > 0 && removed[len(removed)-1] != '\n' {
		linesRemoved++
	}

	return &RewindResult{
		JSONLPath:    path,
		TurnsRemoved: n,
		LinesRemoved: linesRemoved,
		BytesRemoved: int64(len(removed)),
		BackupPath:   backup,
	}, nil
}

// isUserPromptLine reports whether a transcript line is a genuine user prompt.
// The transcript records tool results with role "user" too, so a line counts
// as a turn boundary only when its message content is a plain string or an
// array containing a text block — never a bare tool_result.
func isUserPromptLine(line []byte) bool {
	var e struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return false
	}
	if e.Type != "user" {
		return false
	}
	// content as a plain string is always a real prompt.
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		return true
	}
	// content as an array is a real prompt iff it carries a text block.
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(e.Message.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" {
				return true
			}
		}
	}
	return false
}
