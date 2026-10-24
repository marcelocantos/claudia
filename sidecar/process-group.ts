// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Seat-owned tool commands run in their own POSIX process group (🎯T219 /
// 🎯jevons/T1143), never the broker/sidecar group. Teardown uses the group
// leader pid (negative signal), not kill(-1) and not the supervisor's PG.

const killGraceMs = 2_000;

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
