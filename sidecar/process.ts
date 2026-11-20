// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { spawn } from "node:child_process";

// Group leaders this process started. A tool child is detached, so it is its
// own session: the sidecar exiting does not deliver SIGHUP, and a background
// job the shell left in that group (sleep 60 &) keeps running after the
// sidecar is gone. Abort and timeout already signal the group; exit must too.
//
// killAllGroups reaches every process that stayed in a group this process
// created. A descendant that left the group (setsid, a new session) is
// outside it — the same bound as the abort path's kill of -pid. This does
// not walk the process tree and does not kill a pid it did not spawn.
const groups = new Set<number>();

export function killAllGroups(): void {
  if (process.platform === "win32") return;
  for (const pid of [...groups]) {
    groups.delete(pid);
    try {
      process.kill(-pid, "SIGKILL");
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "ESRCH") {
        try { process.kill(pid, "SIGKILL"); } catch { /* already gone */ }
      }
    }
  }
}

// RunResult is what runCommand saw, shaped like a synchronous spawn's result
// so the tools read it the way they always did.
export type RunResult = {
  stdout: string;
  stderr: string;
  status: number | null;
  error?: NodeJS.ErrnoException;
};

// runCommand runs a child without blocking the event loop (🎯T161).
//
// The tools ran their children synchronously, and the sidecar is one Bun
// process serving every seat: one seat's 60 s build froze every other seat's
// prompt, load and adopt until it returned. Across a broker restart that was
// minutes of nothing — the spool went silent for 60-124 s at a time while
// loads queued behind Bash, and resumes that waited 2 s for an adopt reply
// gave up and relaunched. The bounds are unchanged: past timeoutMs, or past
// maxBuffer bytes on either stream, the child is killed and the result
// carries an error.
export function runCommand(
  command: string,
  args: string[],
  opts: { cwd?: string; timeoutMs: number; maxBuffer: number; env?: NodeJS.ProcessEnv; signal?: AbortSignal; onSpawn?: (pid: number) => void },
): Promise<RunResult> {
  return new Promise((resolve) => {
    const out: Buffer[] = [];
    const errOut: Buffer[] = [];
    let outBytes = 0;
    let errBytes = 0;
    let error: NodeJS.ErrnoException | undefined;
    let settled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let child: ReturnType<typeof spawn> | undefined;
    const onAbort = () => fail("ABORT_ERR", `${command} aborted`);
    const finish = (status: number | null) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      opts.signal?.removeEventListener("abort", onAbort);
      resolve({
        stdout: Buffer.concat(out).toString("utf8"),
        stderr: Buffer.concat(errOut).toString("utf8"),
        status,
        error,
      });
    };
    const fail = (code: string, message: string) => {
      if (!error) {
        error = Object.assign(new Error(message), { code }) as NodeJS.ErrnoException;
      }
      // Each command owns a process group: killing only bash leaves `sleep`
      // (and pipe-holding grandchildren) running after the turn is cancelled.
      if (child?.pid && process.platform !== "win32") {
        try { process.kill(-child.pid, "SIGKILL"); } catch (e) {
          if ((e as NodeJS.ErrnoException).code !== "ESRCH") child.kill("SIGKILL");
        }
      } else child?.kill("SIGKILL");
      // A grandchild holding the pipes open must not hold the seat.
      child?.stdout?.destroy();
      child?.stderr?.destroy();
      finish(null);
    };
    if (opts.signal?.aborted) {
      fail("ABORT_ERR", `${command} aborted`);
      return;
    }
    child = spawn(command, args, { cwd: opts.cwd, env: opts.env, detached: process.platform !== "win32", stdio: ["ignore", "pipe", "pipe"] });
    const leader = child.pid;
    if (leader && process.platform !== "win32") groups.add(leader);
    if (leader) opts.onSpawn?.(leader);
    opts.signal?.addEventListener("abort", onAbort, { once: true });
    if (opts.signal?.aborted) onAbort();
    if (!settled) timer = setTimeout(() => fail("ETIMEDOUT", `${command} timed out after ${opts.timeoutMs}ms`), opts.timeoutMs);
    const collect = (into: Buffer[], add: (n: number) => number) => (chunk: Buffer) => {
      if (settled) return;
      if (add(chunk.length) > opts.maxBuffer) {
        fail("ENOBUFS", `${command} output exceeded ${opts.maxBuffer} bytes`);
        return;
      }
      into.push(chunk);
    };
    child.stdout?.on("data", collect(out, (n) => (outBytes += n)));
    child.stderr?.on("data", collect(errOut, (n) => (errBytes += n)));
    child.on("error", (err: NodeJS.ErrnoException) => {
      if (!settled) error = err;
      finish(null);
    });
    child.on("close", (code) => {
      if (leader) groups.delete(leader);
      finish(code);
    });
  });
}

