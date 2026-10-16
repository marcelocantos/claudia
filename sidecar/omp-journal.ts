// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { createHash } from "node:crypto";
import { constants, lstatSync, mkdirSync, openSync, readSync, closeSync } from "node:fs";
import { dirname, isAbsolute, join, resolve } from "node:path";
import { CURRENT_SESSION_VERSION } from "@oh-my-pi/pi-coding-agent/session/session-entries";
import { loadSessionFile } from "@oh-my-pi/pi-coding-agent/session/session-loader";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { FileSessionStorage, type SessionStorage } from "@oh-my-pi/pi-coding-agent/session/session-storage";
import type { FactJournal } from "./journal.ts";
import { ReceiptJournal } from "./receipts.ts";
import { UsageJournal } from "./usage.ts";

const bindingType = "claudia_session_v1";
const identityByteLimit = 1024;
type Identity = { seat: string; session: string; omp_session_id: string };
type Location = { seat: string; session: string; path: string };
const component = (value: string) => createHash("sha256").update(value).digest("hex");

function location(root: string, seat: string, session: string): Location {
  if (typeof root !== "string" || !isAbsolute(root)) throw new Error("OMP journal requires an explicit absolute state root");
  for (const value of [seat, session]) {
    if (typeof value !== "string" || !value.trim() || Buffer.byteLength(value) > identityByteLimit) {
      throw new Error("OMP journal: invalid seat/session identity");
    }
  }
  return { seat, session, path: join(resolve(root), component(seat), `${component(session)}.jsonl`) };
}

function stamp(path: string): string {
  const s = lstatSync(path, { bigint: true });
  if (!s.isFile() || s.isSymbolicLink()) throw new Error("OMP journal: source is not a regular file");
  return `${s.dev}:${s.ino}:${s.size}:${s.mtimeNs}:${s.ctimeNs}`;
}

// OMP's tolerant interactive loader may skip malformed records and rewrite
// them away later. Service receipts cannot disappear that way. Reuse its
// parser, but reject its corruption diagnostics before opening a writer.
async function validateStored(store: Location, storage: FileSessionStorage): Promise<string> {
  const before = stamp(store.path);
  const fd = openSync(store.path, constants.O_RDONLY | constants.O_NOFOLLOW);
  try {
    const size = storage.statSync(store.path).size;
    const tail = Buffer.alloc(1);
    if (!size || readSync(fd, tail, 0, 1, size - 1) !== 1 || tail[0] !== 10) {
      throw new Error("OMP journal: empty or incomplete journal; refusing repair");
    }
  } finally { closeSync(fd); }
  const loaded = await loadSessionFile(store.path, storage, { throwIfMissing: true });
  if (loaded.invalidHeader || loaded.malformedRecords || loaded.entries.length === 0) {
    throw new Error("OMP journal: malformed journal; refusing repair");
  }
  const header = loaded.entries[0];
  if (header.type !== "session" || header.version !== CURRENT_SESSION_VERSION || !header.id) {
    throw new Error("OMP journal: unsupported session header; refusing migration");
  }
  const ids = new Set<string>();
  let binding: Identity | undefined;
  for (const entry of loaded.entries.slice(1)) {
    if (entry.type === "session" || typeof entry.id !== "string" || !entry.id || ids.has(entry.id) ||
        (entry.parentId !== null && !ids.has(entry.parentId)) || !Number.isFinite(Date.parse(entry.timestamp))) {
      throw new Error("OMP journal: invalid entry tree");
    }
    ids.add(entry.id);
    if ("customType" in entry && entry.customType === bindingType) {
      if (entry.type !== "custom") throw new Error("OMP journal: invalid identity binding carrier");
      if (binding) throw new Error("OMP journal: duplicate identity binding");
      binding = entry.data as Identity;
    }
  }
  if (!binding || binding.seat !== store.seat || binding.session !== store.session || binding.omp_session_id !== header.id) {
    throw new Error("OMP journal: missing or foreign identity binding");
  }
  const readonlyFacts: FactJournal = { store, entries: loaded.entries.slice(1),
    appendFact: () => { throw new Error("OMP journal validation cannot write"); } };
  // Reuse the service projections' schema/transition validation before a
  // writable SDK loader can sanitize or rewrite any portion of this journal.
  new ReceiptJournal(readonlyFacts);
  new UsageJournal(readonlyFacts);
  if (before !== stamp(store.path)) throw new Error("OMP journal: source changed while validating");
  return before;
}

// This is a service adapter over the SDK's sole journal, not a transcript
// implementation. It adds identity admission and a checked durability barrier
// for facts. Only file storage has the synchronous publish contract used here.
export class OMPJournal implements FactJournal {
  #closed = false;
  #failure?: Error;
  #unsubscribe: () => void;
  private constructor(readonly store: Location, readonly manager: SessionManager, readonly restored: boolean) {
    this.#unsubscribe = manager.onPersistenceError(error => { this.#failure ??= error; });
  }

  static async open(options: {
    root: string; seat: string; session: string; cwd: string; requireResume?: boolean;
    storage?: FileSessionStorage;
  }): Promise<OMPJournal> {
    const store = location(options.root, options.seat, options.session);
    if (typeof options.cwd !== "string" || !isAbsolute(options.cwd)) throw new Error("OMP journal requires an explicit absolute working directory");
    const storage = options.storage ?? new FileSessionStorage();
    if (!(storage instanceof FileSessionStorage) || (storage as SessionStorage).defersSyncPublish) {
      throw new Error("OMP journal requires synchronous file storage");
    }
    for (const dir of [resolve(options.root), dirname(store.path)]) {
      if (!options.requireResume) mkdirSync(dir, { recursive: true, mode: 0o700 });
      const stat = lstatSync(dir);
      if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error("OMP journal: storage directory is not a real directory");
    }
    let existing = false;
    try { stamp(store.path); existing = true; }
    catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
    if (!existing && options.requireResume) throw new Error("OMP journal: required session is missing");
    const validated = existing ? await validateStored(store, storage) : undefined;
    const manager = await SessionManager.open(store.path, dirname(store.path), storage, {
      initialCwd: options.cwd, suppressBreadcrumb: true, throwIfMissing: existing,
    });
    const journal = new OMPJournal(store, manager, existing);
    try {
      if (validated !== undefined && validated !== stamp(store.path)) throw new Error("OMP journal: source changed while opening");
      await manager.ensureOnDisk();
      if (!existing) journal.appendFact(bindingType, { seat: store.seat, session: store.session, omp_session_id: manager.getSessionId() });
      await journal.flush();
      return journal;
    } catch (error) {
      try { await journal.close(); }
      catch (cleanup) { throw new AggregateError([error, cleanup], "OMP journal initialization and cleanup failed"); }
      throw error;
    }
  }

  get entries() { return this.manager.getEntries(); }

  assertHealthy(): void {
    if (this.#closed) throw new Error("OMP journal is closed");
    if (this.#failure) throw new Error(`OMP journal persistence failed: ${this.#failure.message}`);
  }

  appendFact(customType: string, data: unknown): void {
    this.assertHealthy();
    if (typeof customType !== "string" || !customType.trim()) throw new Error("OMP journal: missing fact type");
    try {
      const id = this.manager.appendCustomEntry(customType, structuredClone(data));
      // A sealed SDK manager silently drops appends. Never acknowledge one.
      if (!this.manager.getEntry(id)) throw new Error("OMP journal: session no longer accepts entries");
      this.manager.flushSync();
      this.assertHealthy();
    } catch (error) {
      this.#failure ??= error instanceof Error ? error : new Error(String(error));
      throw this.#failure;
    }
  }

  async flush(): Promise<void> {
    this.assertHealthy();
    try { await this.manager.flush(); this.assertHealthy(); }
    catch (error) { this.#failure ??= error instanceof Error ? error : new Error(String(error)); throw this.#failure; }
  }

  async close(): Promise<void> {
    if (this.#closed) return;
    this.#closed = true;
    this.manager.seal();
    try { await this.manager.close(); }
    finally { this.#unsubscribe(); }
  }
}
