// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from "node:child_process";

// Seat-owned tool commands run in their own POSIX process group (🎯T219 /
// 🎯jevons/T1143), never the broker/sidecar group. Teardown uses the group
// leader pid (negative signal), not kill(-1) and not the supervisor's PG.

const killGraceMs = 2_000;

/** True when any process still answers in the seat-owned group (leader pid = pgid). */
export function processGroupHasLiveMembers(pgid: number): boolean {
  if (!pgid || pgid < 1) return false;
  if (process.platform === "win32") {
    try {
      process.kill(pgid, 0);
      return true;
    } catch {
      return false;
    }
  }
  try {
    process.kill(-pgid, 0);
    return true;
  } catch {
    return false;
  }
}

/** PIDs presently in the process group, for reuse guards after the leader exits. */
export function listProcessGroupPids(pgid: number): number[] {
  if (!pgid || pgid < 1 || process.platform === "win32") return [];
  try {
    const out = execFileSync("pgrep", ["-g", String(pgid)], { encoding: "utf8" }).trim();
    if (!out) return [];
    return out
      .split("\n")
      .map((line) => Number.parseInt(line, 10))
      .filter((n) => Number.isFinite(n) && n > 0);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    const status = (err as { status?: number }).status;
    if (code === "ENOENT" || status === 1) return [];
    return [];
  }
}

/**
 * Whether a retained pgid still refers to seat-owned descendants, not a reused id.
 * Witness PIDs were captured when the shell leader exited but the group stayed live.
 */
export function retainedProcessGroupStillOwned(pgid: number, witnessPids: number[]): boolean {
  if (!processGroupHasLiveMembers(pgid)) return false;
  const current = listProcessGroupPids(pgid);
  if (current.length === 0) return false;
  const witness = new Set(witnessPids);
  for (const pid of current) {
    if (witness.has(pid)) return true;
  }
  return false;
}

function signalGroup(pid: number, sig: NodeJS.Signals): void {
  if (!pid || pid < 1) return;
  if (process.platform === "win32") {
    try {
      process.kill(pid, sig);
    } catch {
      /* ESRCH */
    }
    return;
  }
  try {
    process.kill(-pid, sig);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ESRCH") return;
    try {
      process.kill(pid, sig);
    } catch {
      /* ESRCH */
    }
  }
}

/** SIGTERM then bounded SIGKILL for an entire seat-owned command tree. */
export function killSeatCommandTree(pid: number): void {
  signalGroup(pid, "SIGTERM");
  setTimeout(() => signalGroup(pid, "SIGKILL"), killGraceMs);
}

/** Spawn options: new session / process group on Unix; unchanged on Windows. */
export function seatCommandSpawnOptions(): { detached: boolean } {
  return { detached: process.platform !== "win32" };
}
