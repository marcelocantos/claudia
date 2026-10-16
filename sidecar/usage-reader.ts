// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { open, stat } from "node:fs/promises";
import type { BigIntStats } from "node:fs";
import { setImmediate as yieldIO } from "node:timers/promises";
import { isDeepStrictEqual } from "node:util";
import { SeatStore, validateStoreEntry, validateStoreHeader } from "./store.ts";
import { usageFactType, usagePageLimit, validateUsageFact, validateUsageTransition, type UsageFact, type UsagePage } from "./usage.ts";

// Admission ceilings, not claims about a provider's history or exact JS heap.
// A projection is charged four times its encoded bytes plus 512 bytes/fact
// for objects/indexes; fact count is bounded separately. One cold scan can
// coexist with the cache and one bounded record buffer. Oversize journals
// fail explicitly; they are never truncated or repaired by this reader.
export const historicalUsageLimits = {
  fileBytes: 256 * 1024 * 1024,
  recordBytes: 8 * 1024 * 1024,
  facts: 50_000,
  projectionCharge: 16 * 1024 * 1024,
  cacheCharge: 32 * 1024 * 1024,
  cacheEntries: 8,
  concurrentScans: 1,
  readBytes: 64 * 1024,
  yieldRecords: 256,
  deadlineMs: 10_000,
};
type Limits = typeof historicalUsageLimits;
type Query = { expiresAt?: number; signal?: AbortSignal };
type Projection = { version: string; facts: UsageFact[]; charge: number };

function version(s: BigIntStats): string {
  if (!s.isFile()) throw new Error("usage journal is not a regular file");
  return `${s.dev}:${s.ino}:${s.size}:${s.mtimeNs}:${s.ctimeNs}`;
}

// This is only a bounded projection of canonical facts. It owns no store,
// History, model messages or provider. Eviction loses prior-prefix mutation
// detection; it never loses durable facts, which remain in the source file.
export class HistoricalUsageReader {
  #limits: Limits;
  #cache = new Map<string, Projection>();
  #scans = new Map<string, Promise<Projection>>();
  #bytesRead = 0;
  #scanCount = 0;

  constructor(limits: Partial<Limits> = {}) {
    this.#limits = { ...historicalUsageLimits, ...limits };
    for (const n of Object.values(this.#limits)) {
      if (!Number.isSafeInteger(n) || n < 1) throw new Error("invalid historical usage limit");
    }
  }

  // Diagnostics count actual reads, not inferred work. No transcript data.
  stats() {
    return { bytesRead: this.#bytesRead, scans: this.#scanCount, activeScans: this.#scans.size,
      cacheEntries: this.#cache.size, cacheCharge: [...this.#cache.values()].reduce((n, p) => n + p.charge, 0) };
  }

  async page(store: SeatStore, after = 0, limit = usagePageLimit, query: Query = {}): Promise<UsagePage> {
    if (!Number.isSafeInteger(after) || after < 0 || !Number.isSafeInteger(limit) || limit < 1 || limit > usagePageLimit) {
      throw new Error("invalid usage page cursor or limit");
    }
    if (query.expiresAt !== undefined && (!Number.isSafeInteger(query.expiresAt) || query.expiresAt <= 0)) {
      throw new Error("invalid usage query deadline");
    }
    const deadline = Math.min(query.expiresAt ?? Infinity, Date.now() + this.#limits.deadlineMs);
    const check = () => {
      if (query.signal?.aborted) throw new Error("usage query cancelled");
      if (Date.now() >= deadline) throw new Error("usage query deadline exceeded");
    };
    check();
    const key = JSON.stringify([store.path, store.seat, store.session]);
    const observed = version(await stat(store.path, { bigint: true }));
    check();
    const prior = this.#cache.get(key);
    let projection = prior;
    if (!projection || projection.version !== observed) {
      let pending = this.#scans.get(key);
      if (!pending) {
        if (this.#scans.size >= this.#limits.concurrentScans) throw new Error("historical usage reader busy");
        pending = this.#scan(store, prior, check);
        this.#scans.set(key, pending);
        // Keep cleanup attached even if the requesting socket disappears.
        void pending.finally(() => this.#scans.delete(key)).catch(() => {});
      }
      // A joined reader has its own deadline; it cannot wait forever behind
      // an earlier caller. Cancelling the scan's initiating caller fails it.
      projection = await new Promise<Projection>((resolve, reject) => {
        const cancel = () => reject(new Error("usage query cancelled"));
        const timer = setTimeout(() => reject(new Error("usage query deadline exceeded")), Math.max(1, deadline - Date.now()));
        query.signal?.addEventListener("abort", cancel, { once: true });
        pending!.then(resolve, reject).finally(() => {
          clearTimeout(timer); query.signal?.removeEventListener("abort", cancel);
        });
        if (query.signal?.aborted) cancel();
      });
    }
    check();
    // A rename or append between scanning and serving must not masquerade
    // as a stable read. A later explicit query can rebuild once; no loop.
    if (version(await stat(store.path, { bigint: true })) !== projection.version) throw new Error("usage journal changed while reading");
    check();
    if (after > projection.facts.length) throw new Error("invalid usage page cursor or limit");
    this.#cache.delete(key);
    this.#cache.set(key, projection);
    while (this.#cache.size > this.#limits.cacheEntries || this.stats().cacheCharge > this.#limits.cacheCharge) {
      this.#cache.delete(this.#cache.keys().next().value!);
    }
    const facts = structuredClone(projection.facts.slice(after, after + limit));
    return { seat: store.seat, session_id: store.session, facts, next: after + facts.length, more: after + facts.length < projection.facts.length };
  }

  async #scan(store: SeatStore, prior: Projection | undefined, check: () => void): Promise<Projection> {
    this.#scanCount++;
    const file = await open(store.path, "r");
    try {
      check();
      const initial = await file.stat({ bigint: true });
      const identity = version(initial);
      if (initial.size > BigInt(this.#limits.fileBytes)) throw new Error("usage journal exceeds scan byte limit");
      if (version(await stat(store.path, { bigint: true })) !== identity) throw new Error("usage journal changed while opening");
      const facts: UsageFact[] = [];
      const last = new Map<string, UsageFact>();
      let charge = 0, bytes = 0, line = 0, recordBytes = 0;
      let header = false;
      let fragments: Buffer[] = [];
      const buffer = Buffer.allocUnsafe(this.#limits.readBytes);
      for (;;) {
        check();
        const read = await file.read(buffer, 0, buffer.length, null);
        if (!read.bytesRead) break;
        this.#bytesRead += read.bytesRead;
        bytes += read.bytesRead;
        if (bytes > this.#limits.fileBytes) throw new Error("usage journal exceeds scan byte limit");
        let begin = 0;
        for (let i = 0; i < read.bytesRead; i++) {
          if (buffer[i] !== 10) continue;
          recordBytes += i - begin;
          if (recordBytes > this.#limits.recordBytes) throw new Error("usage journal exceeds record byte limit");
          const raw = fragments.length ? Buffer.concat([...fragments, buffer.subarray(begin, i)], recordBytes).toString("utf8") : buffer.toString("utf8", begin, i);
          fragments = []; recordBytes = 0; begin = i + 1; line++;
          if (raw !== "") {
            let value: unknown;
            try { value = JSON.parse(raw); } catch { throw new Error(`conversation store ${store.path} line ${line} is not JSON`); }
            if (!header) {
              validateStoreHeader(value, store.seat, store.session, store.path);
              header = true;
            } else {
              const entry = validateStoreEntry(value, store.path, line);
              if (entry.type === "custom" && entry.customType === usageFactType) {
                const f = validateUsageFact(entry.data, store.seat, store.session);
                validateUsageTransition(f, facts.length + 1, last.get(f.call_id));
                if (prior && facts.length < prior.facts.length && !isDeepStrictEqual(f, prior.facts[facts.length])) {
                  throw new Error("usage journal changed an existing fact");
                }
                charge += Buffer.byteLength(JSON.stringify(f)) * 4 + 512;
                if (facts.length >= this.#limits.facts || charge > this.#limits.projectionCharge || charge > this.#limits.cacheCharge) {
                  throw new Error("usage journal exceeds fact projection limit");
                }
                facts.push(f); last.set(f.call_id, f);
              }
            }
          }
          if (line % this.#limits.yieldRecords === 0) { await yieldIO(); check(); }
        }
        if (begin < read.bytesRead) {
          recordBytes += read.bytesRead - begin;
          if (recordBytes > this.#limits.recordBytes) throw new Error("usage journal exceeds record byte limit");
          fragments.push(Buffer.from(buffer.subarray(begin, read.bytesRead)));
        }
        // Yield even for a record spanning several chunks; deadlines and
        // unrelated sockets must progress while a large line is assembled.
        await yieldIO();
      }
      check();
      if (!header) throw new Error(`conversation store ${store.path} is empty`);
      if (recordBytes) throw new Error(`conversation store ${store.path} has an incomplete tail; usage is unknown`);
      if (prior && facts.length < prior.facts.length) throw new Error("usage journal removed existing facts");
      if (version(await file.stat({ bigint: true })) !== identity || version(await stat(store.path, { bigint: true })) !== identity) {
        throw new Error("usage journal changed while reading");
      }
      check();
      return { version: identity, facts, charge };
    } finally {
      await file.close();
    }
  }
}
