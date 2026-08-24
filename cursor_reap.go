// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// CursorACPSessionDir is ~/.cursor/acp-sessions/<sessionID>.
func CursorACPSessionDir(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cursor", "acp-sessions", sessionID)
}

// CursorACPStorePath is the SQLite file a cursor-agent ACP client opens
// for sessionID.
func CursorACPStorePath(sessionID string) string {
	dir := CursorACPSessionDir(sessionID)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "store.db")
}

func cursorACPStoreExists(sessionID string) bool {
	p := CursorACPStorePath(sessionID)
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

// shouldKillStoreWriter reports whether pid is safe to signal when
// reaping leftover store.db writers. Self-children of this coordinator
// are included: a failed session/load that dropped stdout without dying
// is exactly the leftover the next Launch must not stack on (🎯T541.1).
func shouldKillStoreWriter(pid, extraPID, selfPID int) bool {
	_ = extraPID
	return pid > 1 && pid != selfPID
}

// listStoreWriters and killPID are seams so hermetics do not need lsof.
var listStoreWriters = listStoreWritersLsof
var killPIDFn = killPIDImpl
var listOrphanCursorACP = listOrphanCursorACPPs

// ReapCursorACPLeftovers kills leftover writers on sessionID's store.db
// plus extraPID when it is still alive. extraPID is the persisted
// ConnectPID from a previous coordinator that cannot adopt stdio.
// Returns the PIDs that were signalled.
func ReapCursorACPLeftovers(sessionID string, extraPID int) []int {
	seen := map[int]struct{}{}
	var out []int
	add := func(pid int) {
		if pid <= 1 {
			return
		}
		if _, ok := seen[pid]; ok {
			return
		}
		seen[pid] = struct{}{}
		killPIDFn(pid)
		out = append(out, pid)
	}
	self := os.Getpid()
	if extraPID > 1 && extraPID != self {
		add(extraPID)
	}
	if sessionID != "" {
		for _, pid := range listStoreWriters(CursorACPStorePath(sessionID)) {
			if !shouldKillStoreWriter(pid, extraPID, self) {
				continue
			}
			add(pid)
		}
	}
	if len(out) > 0 {
		slog.Info("reaped leftover cursor-acp writers",
			"session", sessionID, "pids", fmt.Sprint(out))
	}
	return out
}

// ReapOrphanCursorACP kills ppid=1 `cursor-agent … acp` leftovers from a
// coordinator that exited without Stop (SIGHUP upgrade-exit). Interactive
// `agent --force resume` seats are not matched.
func ReapOrphanCursorACP() []int {
	pids := listOrphanCursorACP()
	var out []int
	for _, pid := range pids {
		if pid <= 1 {
			continue
		}
		killPIDFn(pid)
		out = append(out, pid)
	}
	if len(out) > 0 {
		slog.Info("reaped orphan cursor-acp processes", "n", len(out), "pids", fmt.Sprint(out))
	}
	return out
}

// IsOrphanCursorACP reports whether a ps row is a leftover ACP child
// (parent 1, cursor-agent binary, acp mode). Used by the boot reap and
// by hermetics that feed fixture lines.
func IsOrphanCursorACP(ppid int, command string) bool {
	if ppid != 1 {
		return false
	}
	if !strings.Contains(command, "cursor-agent") {
		return false
	}
	// Mode word is `acp`. Avoid matching `--approve-mcps` alone.
	return strings.Contains(command, " acp") || strings.HasSuffix(command, " acp")
}

func listStoreWritersLsof(path string) []int {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	// WAL/SHM sit next to store.db; lsof on the db inode is enough on
	// Darwin when the process has the handle. Probe wal too.
	var pids []int
	seen := map[int]struct{}{}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		out, err := exec.Command("lsof", "-t", "--", p).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			pid, err := strconv.Atoi(line)
			if err != nil || pid <= 1 {
				continue
			}
			if _, ok := seen[pid]; ok {
				continue
			}
			seen[pid] = struct{}{}
			pids = append(pids, pid)
		}
	}
	return pids
}

func listOrphanCursorACPPs() []int {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range bytes.Split(out, []byte("\n")) {
		s := strings.TrimSpace(string(line))
		if s == "" {
			continue
		}
		pid, ppid, cmd, ok := parsePSLine(s)
		if !ok || !IsOrphanCursorACP(ppid, cmd) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

func parsePSLine(line string) (pid, ppid int, command string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return 0, 0, "", false
	}
	var err error
	pid, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, "", false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, "", false
	}
	command = strings.TrimSpace(line[len(fields[0])+1:])
	// command still starts with ppid; strip it.
	if i := strings.IndexAny(command, " \t"); i >= 0 {
		command = strings.TrimSpace(command[i+1:])
	}
	return pid, ppid, command, true
}

func killPIDImpl(pid int) {
	if pid <= 1 {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Kill()
}

func reapCursorACPDef(def *AgentDef) {
	if def == nil || def.Provider != ProviderCursor {
		return
	}
	ReapCursorACPLeftovers(def.SessionID, def.ConnectPID)
}
