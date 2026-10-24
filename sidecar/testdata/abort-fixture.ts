// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connect, type Socket } from "node:net";

// Cold vendor imports belong to fixture setup, not the abort/send contract.
// This is a bounded allowance for setup, not a runtime performance claim.
export const FIXTURE_STARTUP_MS = 30_000;
export const FIXTURE_CLEANUP_MS = 5_000;
export const ABORT_SEMANTIC_MS = 5_000;
export const FIXTURE_READY = "abort-fixture-ready";
const DIAGNOSTIC_BYTES = 16 * 1024;

export type Ev = Record<string, any>;
type Waiter = { pred: (ev: Ev) => boolean; resolve: (ev: Ev) => void; reject: (error: Error) => void; timer?: ReturnType<typeof setTimeout> };

export class Host {
  events: Ev[] = [];
  #buf = "";
  #failure?: Error;
  #waiters: Waiter[] = [];
  constructor(private socket: Socket) {
    socket.on("error", error => this.#fail(error));
    socket.on("end", () => {
      this.#fail(new Error("fixture host connection closed"));
      socket.destroy();
    });
    socket.on("close", () => this.#fail(new Error("fixture host connection closed")));
    socket.on("data", chunk => {
      this.#buf += chunk.toString("utf8");
      let nl: number;
      while ((nl = this.#buf.indexOf("\n")) >= 0) {
        let ev: Ev;
        try { ev = JSON.parse(this.#buf.slice(0, nl)); }
        catch (error) { this.#fail(new Error(`invalid fixture event: ${error}`)); this.socket.destroy(); return; }
        this.#buf = this.#buf.slice(nl + 1);
        this.events.push(ev);
        for (const waiter of [...this.#waiters]) {
          if (waiter.pred(ev)) {
            this.#remove(waiter);
            waiter.resolve(ev);
          }
        }
      }
    });
  }
  #remove(waiter: Waiter) {
    clearTimeout(waiter.timer);
    this.#waiters = this.#waiters.filter(w => w !== waiter);
  }
  #fail(error: Error) {
    this.#failure ??= error;
    for (const waiter of [...this.#waiters]) {
      this.#remove(waiter);
      waiter.reject(error);
    }
  }
  send(...lines: object[]) {
    if (this.#failure) throw this.#failure;
    this.socket.write(lines.map(l => JSON.stringify(l) + "\n").join(""));
  }
  // The test body owns the semantic deadline. Preserve a narrower bound only
  // where an individual assertion explicitly requests one.
  next(what: string, pred: (ev: Ev) => boolean, ms?: number): Promise<Ev> {
    const seen = this.events.find(pred);
    if (seen) return Promise.resolve(seen);
    if (this.#failure) return Promise.reject(this.#failure);
    return new Promise((resolve, reject) => {
      const waiter: Waiter = { pred, resolve, reject };
      if (ms !== undefined) waiter.timer = setTimeout(() => {
        this.#remove(waiter);
        reject(new Error(`no ${what} within ${ms}ms; saw ${JSON.stringify(this.events.map(e => [e.type, e.turn_id, e.request_id]))}`));
      }, ms);
      this.#waiters.push(waiter);
    });
  }
  close() {
    this.#fail(new Error("fixture host closed by cleanup"));
    this.socket.destroy();
  }
}

// Only test entrypoints are accepted here. No broker, provider or installed
// service is launched. The returned handle owns exactly this child and dir.
export function launchAbortFixture(options: { entrypoint?: string; args?: string[]; signal?: AbortSignal } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "abrt-"));
  const sock = join(dir, "s.sock");
  const child = spawn(process.execPath, [options.entrypoint ?? join(import.meta.dir, "abort-provider.ts"), sock, ...options.args ?? []], {
    stdio: ["pipe", "pipe", "pipe"],
    env: { ...process.env, HOME: dir, CLAUDIA_OMP_SPOOL_DIR: join(dir, "spool") },
  });
  let stderr = "";
  child.stderr.on("data", chunk => { stderr = (stderr + chunk.toString()).slice(-DIAGNOSTIC_BYTES); });
  const exited = new Promise<string>(resolve => {
    child.once("error", error => resolve(`spawn error: ${error.message}`));
    child.once("exit", (code, signal) => resolve(`exit=${code} signal=${signal ?? "none"}`));
  });
  const controller = new AbortController();
  const cancel = () => controller.abort(options.signal?.reason ?? new Error("fixture startup cancelled"));
  options.signal?.addEventListener("abort", cancel, { once: true });
  if (options.signal?.aborted) cancel();
  const watchdog = setTimeout(() => controller.abort(new Error(`fixture startup exceeded ${FIXTURE_STARTUP_MS}ms`)), FIXTURE_STARTUP_MS);
  let host: Host | undefined;
  let socket: Socket | undefined;
  let disposed: Promise<void> | undefined;
  const dispose = () => disposed ??= (async () => {
    host?.close();
    socket?.destroy();
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
    let cleanupTimer: ReturnType<typeof setTimeout> | undefined;
    try {
      await Promise.race([exited, new Promise<never>((_, reject) => {
        cleanupTimer = setTimeout(() => reject(new Error(`fixture child did not exit within ${FIXTURE_CLEANUP_MS}ms`)), FIXTURE_CLEANUP_MS);
      })]);
      child.stdin.destroy(); child.stdout.destroy(); child.stderr.destroy();
      rmSync(dir, { recursive: true, force: true });
    } finally { clearTimeout(cleanupTimer); }
  })();
  const ready = (async () => {
    let abort: () => void = () => {};
    let output = "";
    let onData: (chunk: Buffer) => void = () => {};
    const failed = new Promise<never>((_, reject) => {
      abort = () => reject(controller.signal.reason);
      controller.signal.addEventListener("abort", abort, { once: true });
      if (controller.signal.aborted) abort();
      void exited.then(status => reject(new Error(`fixture exited before readiness: ${status}; ${stderr}`)));
    });
    const announced = new Promise<void>((resolve, reject) => {
      onData = chunk => {
        output += chunk.toString();
        if (output.length > DIAGNOSTIC_BYTES) return reject(new Error("fixture readiness output exceeded bound"));
        if (output.includes("\n")) {
          if (output.trim() === FIXTURE_READY) resolve();
          else reject(new Error(`unexpected fixture readiness: ${output}`));
        }
      };
      child.stdout.on("data", onData);
    });
    try {
      await Promise.race([announced, failed]);
      socket = connect(sock);
      host = new Host(socket);
      await Promise.race([new Promise<void>((resolve, reject) => {
        socket!.once("connect", resolve);
        socket!.once("error", reject);
      }), failed]);
      return host;
    } catch (error) {
      await dispose();
      throw error;
    } finally {
      clearTimeout(watchdog);
      options.signal?.removeEventListener("abort", cancel);
      controller.signal.removeEventListener("abort", abort);
      child.stdout.off("data", onData);
    }
  })();
  return { dir, child, ready, exited, close: async () => {
    controller.abort(new Error("fixture startup cancelled by cleanup"));
    await dispose();
  } };
}
