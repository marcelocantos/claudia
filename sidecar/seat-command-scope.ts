// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { killSeatCommandTree } from "./process-group.ts";

// Tracks in-flight seat-owned subprocess trees for explicit seat stop/reap
// (server abort/drop). Consumer disconnect does not call killAll (🎯T219).
//
// When a shell leader exits 0 while a background child still lives, runCommand
// untracks the leader but the child remains seat-owned until stop/reap — we
// retain the command's process-group leader for killAll (🎯T219/T223).
export class SeatCommandScope {
  private readonly active = new Set<number>();
  private readonly abandonedGroupLeaders = new Set<number>();

  track(pid: number): void {
    if (pid > 0) this.active.add(pid);
  }

  untrack(pid: number): void {
    if (!this.active.delete(pid)) return;
    if (pid > 0) this.abandonedGroupLeaders.add(pid);
  }

  killAll(): void {
    for (const pid of this.active) killSeatCommandTree(pid);
    for (const pid of this.abandonedGroupLeaders) killSeatCommandTree(pid);
    this.active.clear();
    this.abandonedGroupLeaders.clear();
  }
}
