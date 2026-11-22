// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
// Hermetic reproduction: actual production process wrapper, fake sleep only.
// Run with: bun docs/analysis/turn-delivery-model/Ownership-repro.ts
import { execFileSync, spawn } from "node:child_process";
import { mkdtempSync, readFileSync, existsSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { runCommand, killAllGroups } from "./process.ts";

const alive = (pid: number): boolean => {
  try { process.kill(pid, 0); return true; } catch { return false; }
};
const stop = (pid: number) => { try { process.kill(pid, "SIGKILL"); } catch {} };
const delay = (ms: number) => new Promise(resolve => setTimeout(resolve, ms));
const waitFor = async (predicate: () => boolean) => {
  const until = Date.now() + 5_000;
  while (!predicate()) {
    if (Date.now() > until) throw new Error("fixture condition timed out");
    await delay(10);
  }
};

if (process.argv[2] === "--worker") {
  const marker = process.argv[3];
  await runCommand("/bin/sh", ["-c", 'sleep 60 > /dev/null 2>&1 & echo $! > "$1"; wait', "sh", marker], {
    timeoutMs: 65_000, maxBuffer: 1024,
    onSpawn: pid => writeFileSync(marker + ".leader", String(pid)),
  });
} else {
  const root = execFileSync("scratch-root", { encoding: "utf8" }).trim();
  const dir = mkdtempSync(join(root, "claudia-ownership-repro-"));
  const owned = new Set<number>();
  let worker: ReturnType<typeof spawn> | undefined;
  try {
    const retired = join(dir, "retired.pid");
    let retiredLeader = 0;
    const result = await runCommand("/bin/sh", ["-c", 'sleep 60 > /dev/null 2>&1 & echo $! > "$1"', "sh", retired], {
      timeoutMs: 5_000, maxBuffer: 1024, onSpawn: pid => { retiredLeader = pid; owned.add(-pid); },
    });
    const child = Number(readFileSync(retired, "utf8").trim());
    owned.add(child);
    const before = alive(child);
    killAllGroups();
    await delay(100);
    const after = alive(child);
    console.log(JSON.stringify({ scenario: "leader-exits-with-redirected-background-child", shellStatus: result.status,
      shellExited: !alive(retiredLeader), childAliveBeforeCleanup: before, childAliveAfterCleanup: after }));
    if (result.status !== 0 || !before || !after) throw new Error("retired group leak did not reproduce");
    stop(-retiredLeader); stop(child);

    const active = join(dir, "active.pid");
    let activeLeader = 0;
    const running = runCommand("/bin/sh", ["-c", 'sleep 60 > /dev/null 2>&1 & echo $! > "$1"; wait', "sh", active], {
      timeoutMs: 65_000, maxBuffer: 1024, onSpawn: pid => { activeLeader = pid; owned.add(-pid); },
    });
    await waitFor(() => existsSync(active) && readFileSync(active, "utf8").trim() !== "");
    const activeChild = Number(readFileSync(active, "utf8").trim());
    owned.add(activeChild);
    killAllGroups();
    await running;
    await waitFor(() => !alive(activeChild));
    console.log(JSON.stringify({ scenario: "tracked-active-group-control", childAliveAfterCleanup: alive(activeChild) }));

    const abrupt = join(dir, "abrupt.pid");
    worker = spawn(process.execPath, [import.meta.path, "--worker", abrupt], { stdio: "ignore" });
    const workerExited = new Promise<void>(resolve => worker!.once("exit", () => resolve()));
    await waitFor(() => existsSync(abrupt) && readFileSync(abrupt, "utf8").trim() !== "" && existsSync(abrupt + ".leader"));
    const abruptChild = Number(readFileSync(abrupt, "utf8").trim());
    const abruptLeader = Number(readFileSync(abrupt + ".leader", "utf8").trim());
    owned.add(abruptChild); owned.add(-abruptLeader);
    worker.kill("SIGKILL");
    await workerExited;
    await delay(100);
    console.log(JSON.stringify({ scenario: "wrapper-owner-SIGKILL", ownerExited: true,
      leaderAliveAfterOwnerDeath: alive(abruptLeader), childAliveAfterOwnerDeath: alive(abruptChild) }));
    if (!alive(abruptChild)) throw new Error("abrupt owner death leak did not reproduce");
  } finally {
    if (worker && worker.exitCode === null && worker.signalCode === null) worker.kill("SIGKILL");
    for (const pid of owned) stop(pid);
    rmSync(dir, { recursive: true, force: true });
  }
}
