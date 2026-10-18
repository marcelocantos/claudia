// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { constants } from "node:fs";
import type { Dir } from "node:fs";
import { lstat, open, opendir, realpath } from "node:fs/promises";
import { join, resolve } from "node:path";
import { randomUUID } from "node:crypto";
import { SeatStore, seatRuntimeDirName, validateStoreHeader } from "./store.ts";

// Enumeration retains only two directory streams and one retry page per scan.
// Limits cover scanned entries (not only matching histories), encoded strings,
// active scans, and wall time. Native filesystem calls finish cooperatively.
export const generationLimits = Object.freeze({ page: 128, headerBytes: 16 * 1024,
  cursorBytes: 96, identityBytes: 1024, issueBytes: 512, scans: 4, ttlMS: 60_000, requestMS: 10_000 });
export type GenerationIdentity = { seat: string; session_id: string };
export type GenerationIssue = { path: string; code: string; message: string };
export type GenerationPage = { generations: GenerationIdentity[]; issues: GenerationIssue[];
  cursor: string; more: boolean; complete: boolean };
class DiscoveryError extends Error {
  constructor(readonly reason: string, message: string) { super(message); }
}
type Scan = { id: string; page: number; limit: number; touched: number; root?: Dir; seat?: Dir;
  seatName: string; rootPath: string; ended: boolean; issues: boolean; previous?: { cursor: string; result: GenerationPage } };
const clipped = (s: string) => Buffer.from(s).subarray(0, generationLimits.issueBytes).toString("utf8").replace(/\uFFFD$/, "");

export class GenerationDiscovery {
  #scans = new Map<string, Scan>();
  #busy = false;
  #timer: ReturnType<typeof setInterval>;
  #bytesRead = 0;
  constructor(readonly root: string) {
    this.#timer = setInterval(() => { if (!this.#busy) void this.#expire().catch(err => console.error("generation discovery cleanup failed", err)); }, generationLimits.ttlMS);
    this.#timer.unref();
  }
  get bytesRead(): number { return this.#bytesRead; }
  async #close(s: Scan): Promise<void> {
    // Clear handles before awaiting: expiration and shutdown cannot close twice.
    const dirs = [s.seat, s.root]; s.seat = s.root = undefined;
    for (const dir of dirs) if (dir) await dir.close();
  }
  async #expire(): Promise<void> {
    for (const [id, s] of this.#scans) if (Date.now() - s.touched >= generationLimits.ttlMS) {
      this.#scans.delete(id); await this.#close(s);
    }
  }
  async close(): Promise<void> {
    clearInterval(this.#timer);
    for (const s of this.#scans.values()) await this.#close(s);
    this.#scans.clear();
  }
  async page(cursor = "", limit = generationLimits.page, options: { expiresAt?: number; signal?: AbortSignal } = {}): Promise<GenerationPage> {
    if (typeof cursor !== "string" || Buffer.byteLength(cursor) > generationLimits.cursorBytes ||
        !Number.isInteger(limit) || limit < 1 || limit > generationLimits.page ||
        (options.expiresAt !== undefined && (!Number.isSafeInteger(options.expiresAt) || options.expiresAt < 0))) throw new Error("invalid generation discovery query");
    if (this.#busy) throw new DiscoveryError("discovery_busy", "generation discovery is busy; retry the same cursor");
    this.#busy = true;
    let s: Scan | undefined;
    const deadline = Math.min(options.expiresAt || Infinity, Date.now() + generationLimits.requestMS);
    const check = () => { if (options.signal?.aborted || Date.now() >= deadline) throw new DiscoveryError("discovery_cursor_expired", "generation discovery interrupted; restart from an empty cursor"); };
    try {
      await this.#expire(); check();
      if (cursor) {
        const id = cursor.split(":")[0]; s = this.#scans.get(id);
        if (!s || s.limit !== limit) throw new DiscoveryError("discovery_cursor_expired", "discovery cursor expired or incompatible; restart from an empty cursor");
        s.touched = Date.now();
        if (s.previous?.cursor === cursor) return structuredClone(s.previous.result);
        if (cursor !== `${s.id}:${s.page}` || s.ended) throw new DiscoveryError("discovery_cursor_expired", "discovery cursor is no longer resumable; restart from an empty cursor");
      } else {
        if (this.#scans.size >= generationLimits.scans) throw new DiscoveryError("discovery_busy", "generation discovery capacity reached; wait for cursor expiry");
        s = { id: randomUUID(), page: 0, limit, touched: Date.now(), seatName: "", rootPath: resolve(this.root), ended: false, issues: false };
        this.#scans.set(s.id, s);
      }
      const result: GenerationPage = { generations: [], issues: [], cursor: "", more: true, complete: false };
      const issue = (path: string, code: string, message: string) => {
        s!.issues = true; result.issues.push({ path: clipped(path), code, message: clipped(message) });
      };
      if (!s.root && !s.ended) {
        try {
          const stat = await lstat(this.root); check();
          if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error("history root must be a real directory");
          s.rootPath = await realpath(this.root);
          s.root = await opendir(s.rootPath, { bufferSize: 1 }); check();
        } catch (err) {
          check();
          if ((err as NodeJS.ErrnoException).code !== "ENOENT") issue(".", "unreadable", String(err));
          s.ended = true;
        }
      }
      for (let work = 0; work < limit && !s.ended; work++) {
        check();
        if (!s.seat) {
          const entry = await s.root!.read(); check();
          if (!entry) { s.ended = true; break; }
          // The SDK model cache shares the history root and is not a seat.
          if (entry.name === seatRuntimeDirName && entry.isDirectory() && !entry.isSymbolicLink()) continue;
          if (!entry.isDirectory() || entry.isSymbolicLink()) {
            issue(entry.name, "unexpected_entry", "history root entry is not a regular seat directory"); continue;
          }
          s.seatName = entry.name;
          try {
            const path = join(s.rootPath, entry.name);
            if (await realpath(path) !== resolve(path)) throw new Error("seat directory is a symlink");
            s.seat = await opendir(path, { bufferSize: 1 }); check();
          } catch (err) { check(); issue(entry.name, "unreadable", String(err)); }
          continue;
        }
        const entry = await s.seat.read(); check();
        if (!entry) { await s.seat.close(); s.seat = undefined; continue; }
        const relative = join(s.seatName, entry.name);
        if (!entry.isFile() || entry.isSymbolicLink() || !entry.name.endsWith(".jsonl")) {
          issue(relative, "unexpected_entry", "seat entry is not a regular history file"); continue;
        }
        const path = join(s.rootPath, relative);
        try {
          if (await realpath(path) !== resolve(path)) throw new Error("history path is a symlink");
          const f = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
          try {
            const before = await f.stat(); check();
            if (!before.isFile()) throw new Error("history is not a regular file");
            const bytes = Buffer.alloc(generationLimits.headerBytes);
            const { bytesRead } = await f.read(bytes, 0, bytes.length, 0); this.#bytesRead += bytesRead; check();
            const nl = bytes.subarray(0, bytesRead).indexOf(10);
            if (nl < 0) throw new Error("history header is missing, torn, or exceeds header byte limit");
            const h = JSON.parse(bytes.subarray(0, nl).toString("utf8"));
            if (typeof h?.seat !== "string" || typeof h?.session !== "string" || !h.seat.trim() || !h.session.trim() ||
                Buffer.byteLength(h.seat) > generationLimits.identityBytes || Buffer.byteLength(h.session) > generationLimits.identityBytes) throw new Error("invalid history identity");
            validateStoreHeader(h, h.seat, h.session, relative);
            if (new SeatStore(s.rootPath, h.seat, h.session).path !== path) throw new Error("header identity does not match history path");
            const after = await lstat(path); check();
            if (!after.isFile() || before.dev !== after.dev || before.ino !== after.ino || await realpath(path) !== resolve(path)) throw new Error("history identity changed during header read");
            result.generations.push({ seat: h.seat, session_id: h.session });
          } finally { await f.close(); }
        } catch (err) { check(); issue(relative, "invalid_header", String(err)); }
      }
      check();
      if (s.ended) await this.#close(s);
      result.more = !s.ended; result.complete = s.ended && !s.issues;
      s.page++; result.cursor = result.more ? `${s.id}:${s.page}` : "";
      s.previous = { cursor, result }; s.touched = Date.now();
      return structuredClone(result);
    } catch (err) {
      // A failed in-progress page may have advanced native iterators. Never
      // pretend its old cursor still resumes safely: restart and deduplicate.
      if (s) { this.#scans.delete(s.id); await this.#close(s); }
      throw err;
    } finally { this.#busy = false; }
  }
}
