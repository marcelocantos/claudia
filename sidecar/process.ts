// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { spawn } from "node:child_process";

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
  opts: { cwd?: string; timeoutMs: number; maxBuffer: number; env?: NodeJS.ProcessEnv },
): Promise<RunResult> {
  return new Promise((resolve) => {
    const out: Buffer[] = [];
    const errOut: Buffer[] = [];
    let outBytes = 0;
    let errBytes = 0;
    let error: NodeJS.ErrnoException | undefined;
    let settled = false;
    const finish = (status: number | null) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
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
      child.kill("SIGTERM");
      // A grandchild holding the pipes open must not hold the seat.
      child.stdout?.destroy();
      child.stderr?.destroy();
      finish(null);
    };
    const child = spawn(command, args, { cwd: opts.cwd, env: opts.env, stdio: ["ignore", "pipe", "pipe"] });
    const timer = setTimeout(() => fail("ETIMEDOUT", `${command} timed out after ${opts.timeoutMs}ms`), opts.timeoutMs);
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

