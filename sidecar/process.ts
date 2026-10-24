// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { spawn, type ChildProcess } from "node:child_process";
import { killSeatCommandTree, seatCommandSpawnOptions } from "./process-group.ts";
import type { SeatCommandScope } from "./seat-command-scope.ts";

// RunResult is what runCommand saw, shaped like a synchronous spawn's result
// so the tools read it the way they always did.
export type RunResult = {
  stdout: string;
  stderr: string;
  status: number | null;
  error?: NodeJS.ErrnoException;
};

export type RunCommandOpts = {
  cwd?: string;
  timeoutMs: number;
  maxBuffer: number;
  env?: NodeJS.ProcessEnv;
  signal?: AbortSignal;
  commandScope?: SeatCommandScope;
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
//
// Seat-owned children spawn in a dedicated process group (🎯T219); teardown
// signals the whole group. A host/broker disconnect does not cancel these
// subprocesses — only explicit seat abort/drop (via SeatCommandScope.killAll
// and the turn's AbortSignal on coding tools) or normal timeout/maxBuffer.
export function runCommand(
  command: string,
  args: string[],
  opts: RunCommandOpts,
): Promise<RunResult> {
  return new Promise((resolve) => {
    const out: Buffer[] = [];
    const errOut: Buffer[] = [];
    let outBytes = 0;
    let errBytes = 0;
    let error: NodeJS.ErrnoException | undefined;
    let settled = false;
    let child: ChildProcess | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let onAbort: (() => void) | undefined;

    const finish = (status: number | null) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      if (opts.signal && onAbort) opts.signal.removeEventListener("abort", onAbort);
      resolve({
        stdout: Buffer.concat(out).toString("utf8"),
        stderr: Buffer.concat(errOut).toString("utf8"),
        status,
        error,
      });
    };
    const teardownTree = () => {
      const pid = child?.pid;
      if (pid) killSeatCommandTree(pid);
    };
    const fail = (code: string, message: string) => {
      if (!error) {
        error = Object.assign(new Error(message), { code }) as NodeJS.ErrnoException;
      }
      teardownTree();
      child?.stdout?.destroy();
      child?.stderr?.destroy();
      finish(null);
    };

    child = spawn(command, args, {
      cwd: opts.cwd,
      env: opts.env,
      stdio: ["ignore", "pipe", "pipe"],
      ...seatCommandSpawnOptions(),
    });
    const pid = child.pid;
    if (pid && opts.commandScope) {
      opts.commandScope.track(pid);
      child.on("close", () => opts.commandScope?.untrack(pid));
    }
    timer = setTimeout(() => fail("ETIMEDOUT", `${command} timed out after ${opts.timeoutMs}ms`), opts.timeoutMs);
    if (opts.signal) {
      if (opts.signal.aborted) {
        fail("ABORT_ERR", "seat command aborted");
        return;
      }
      onAbort = () => fail("ABORT_ERR", "seat command aborted");
      opts.signal.addEventListener("abort", onAbort, { once: true });
    }
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
      error = err;
      finish(null);
    });
    child.on("close", (code) => finish(code));
  });
}
