// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { killSeatCommandTree } from "./process-group.ts";

// Tracks in-flight seat-owned subprocess trees for explicit seat stop/reap
// (server abort/drop). Consumer disconnect does not call killAll (🎯T219).
export class SeatCommandScope {
  private readonly pids = new Set<number>();

  track(pid: number): void {
    if (pid > 0) this.pids.add(pid);
  }

  untrack(pid: number): void {
    this.pids.delete(pid);
  }

  killAll(): void {
    for (const pid of this.pids) killSeatCommandTree(pid);
    this.pids.clear();
  }
}
