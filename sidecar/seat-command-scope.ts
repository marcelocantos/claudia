// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import {
  killSeatCommandTree,
  listProcessGroupPids,
  processGroupHasLiveMembers,
  retainedProcessGroupStillOwned,
} from "./process-group.ts";

type RetainedGroup = {
  pgid: number;
  witnessPids: number[];
};

// Tracks in-flight seat-owned subprocess trees for explicit seat stop/reap
// (server abort/drop). Consumer disconnect does not call killAll (🎯T219).
//
// When a shell leader exits 0 while a background child still lives, runCommand
// untracks the leader but the child remains seat-owned until stop/reap — we
// retain the command's process-group id only while live descendants remain,
// with witness PIDs so reused pgids are never signaled (🎯T219/T223).
export class SeatCommandScope {
  private readonly active = new Set<number>();
  private readonly retainedGroups: RetainedGroup[] = [];

  track(pid: number): void {
    if (pid > 0) this.active.add(pid);
  }

  untrack(pid: number): void {
    if (!this.active.delete(pid)) return;
    if (pid <= 0) return;
    if (!processGroupHasLiveMembers(pid)) return;
    const witnessPids = listProcessGroupPids(pid);
    this.retainedGroups.push({ pgid: pid, witnessPids });
  }

  killAll(): void {
    for (const pid of this.active) killSeatCommandTree(pid);
    for (const g of this.retainedGroups) {
      if (!retainedProcessGroupStillOwned(g.pgid, g.witnessPids)) continue;
      killSeatCommandTree(g.pgid);
    }
    this.active.clear();
    this.retainedGroups.length = 0;
  }

  /** Test hook: PIDs that killAll would signal (active leaders + owned retained pgids). */
  pendingKillTargets(): number[] {
    const out = [...this.active];
    for (const g of this.retainedGroups) {
      if (retainedProcessGroupStillOwned(g.pgid, g.witnessPids)) out.push(g.pgid);
    }
    return out;
  }
}
