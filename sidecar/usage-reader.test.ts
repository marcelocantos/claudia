// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, test } from "bun:test";
import { appendFileSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { createServer, connect } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setImmediate as yieldIO } from "node:timers/promises";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";
import { SeatStore } from "./store.ts";
import { HistoricalUsageReader } from "./usage-reader.ts";
import { usageFactType, type UsageFact } from "./usage.ts";

const roots: string[] = [];
afterEach(() => { for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true }); });
function fixture(count = 300, messageBytes = 0) {
  const root = mkdtempSync(join(tmpdir(), "usage-reader-")); roots.push(root);
  const store = new SeatStore(root, "seat", "session");
  const facts: UsageFact[] = Array.from({ length: count }, (_, i) => ({
    seq: i + 1, call_id: `call-${i}`, seat: "seat", session_id: "session", kind: "model", state: "started",
    provider: "mock", model: "mock", requests: [], started_at: "2026-10-16T00:00:00Z", counters: {},
    counter_source: "adapter_normalized_positive", coverage: "engine_invocation",
  }));
  const entries: SessionEntry[] = facts.map(f => ({ type: "custom", id: `f-${f.seq}`, parentId: null,
    timestamp: f.started_at, customType: usageFactType, data: f }));
  // Large non-usage records must be validated then discarded, not retained
  // alongside the fact projection. Their size varies independently of F.
  if (messageBytes) entries.unshift({ type: "message", id: "large", parentId: null, timestamp: facts[0]?.started_at ?? "",
    message: { role: "user", content: "x".repeat(messageBytes), timestamp: 1 } });
  store.rewrite(entries);
  return { root, store, entries, facts };
}
async function whileScanning(reader: HistoricalUsageReader) {
  for (let i = 0; i < 10_000; i++) {
    if (reader.stats().bytesRead > 0 && reader.stats().activeScans > 0) return;
    await yieldIO();
  }
  throw new Error("scan did not start");
}

test("multiple pages read the source once and cache only the bounded fact projection", async () => {
  const f = fixture(300, 1024 * 1024);
  const reader = new HistoricalUsageReader();
  const found: UsageFact[] = [];
  let next = 0;
  do {
    const page = await reader.page(f.store, next);
    found.push(...page.facts); next = page.next;
    if (!page.more) break;
  } while (true);
  expect(found).toEqual(f.facts);
  expect(reader.stats().scans).toBe(1);
  expect(reader.stats().bytesRead).toBe(statSync(f.store.path).size);
  const charge = f.facts.reduce((n, fact) => n + Buffer.byteLength(JSON.stringify(fact)) * 4 + 512, 0);
  expect(reader.stats().cacheCharge).toBe(charge);
  const first = await reader.page(f.store, 0, 1);
  first.facts[0].model = "mutated-client-copy";
  expect((await reader.page(f.store, 0, 1)).facts[0]).toEqual(f.facts[0]);
  expect(reader.stats().scans).toBe(1);
});

test("append and preserving rewrite rebuild while conflicting prefix or truncation fails", async () => {
  const f = fixture(300);
  const reader = new HistoricalUsageReader();
  expect((await reader.page(f.store, 0)).next).toBe(128);
  const extra = { ...f.facts[0], seq: 301, call_id: "appended" };
  const entry = { ...f.entries[0], id: "append", data: extra } as SessionEntry;
  f.store.append([entry]);
  expect((await reader.page(f.store, 128)).facts).toEqual(f.facts.slice(128, 256));
  f.entries.push(entry);
  // Rewrite alters unrelated model history and inode, but keeps facts.
  f.store.rewrite([{ type: "message", id: "replacement", parentId: null, timestamp: "",
    message: { role: "user", content: "replacement context", timestamp: 1 } }, ...f.entries]);
  expect((await reader.page(f.store, 256)).facts).toEqual([...f.facts.slice(256), extra]);
  expect(reader.stats().scans).toBe(3);
  const bad = structuredClone(f.entries);
  (bad[0] as { data: UsageFact }).data.model = "evil"; // same-length replacement
  f.store.rewrite(bad);
  await expect(reader.page(f.store, 256)).rejects.toThrow("changed an existing fact");
  f.store.rewrite(f.entries.slice(0, 100));
  await expect(reader.page(f.store, 0)).rejects.toThrow("removed existing facts");
  f.store.rewrite(f.entries);
  expect((await reader.page(f.store, 300)).facts).toEqual([extra]);
});

test("cold scans share identity and transition checks and never repair bad journals", async () => {
  for (const kind of ["identity", "json", "entry", "sequence", "transition", "torn", "delimiter"] as const) {
    const f = fixture(2);
    let text = readFileSync(f.store.path, "utf8");
    if (kind === "identity") text = text.replace('"seat":"seat"', '"seat":"foreign"');
    if (kind === "json") text += "{invalid}\n";
    if (kind === "entry") text += '{"type":"message"}\n';
    if (kind === "sequence") text = text.replace('"seq":2', '"seq":3');
    if (kind === "transition") text = text.replace('"call_id":"call-1"', '"call_id":"call-0"');
    if (kind === "torn") text += '{"type":';
    if (kind === "delimiter") text = text.slice(0, -1);
    writeFileSync(f.store.path, text);
    const reader = new HistoricalUsageReader();
    await expect(reader.page(f.store)).rejects.toThrow();
    expect(reader.stats().cacheEntries).toBe(0);
    expect(readFileSync(f.store.path, "utf8")).toBe(text);
  }
});

test("source mutation during asynchronous scan never publishes a partial projection", async () => {
  for (const rewrite of [false, true]) {
    const f = fixture(2, 128 * 1024);
    const reader = new HistoricalUsageReader({ readBytes: 64 });
    const result = reader.page(f.store).then(() => "unexpected pass", e => String(e));
    await whileScanning(reader);
    if (rewrite) f.store.rewrite(f.entries);
    else appendFileSync(f.store.path, "\n");
    expect(await result).toContain("changed while reading");
    expect(reader.stats().cacheEntries).toBe(0);
    expect((await reader.page(f.store)).facts).toEqual(f.facts);
  }
});

test("expired, cancelled, oversized and concurrent scans have explicit bounded failure", async () => {
  const f = fixture(3, 128 * 1024);
  const expired = new HistoricalUsageReader();
  await expect(expired.page(f.store, 0, 1, { expiresAt: Date.now() - 1 })).rejects.toThrow("deadline");
  expect(expired.stats().bytesRead).toBe(0);
  for (const limits of [{ fileBytes: 64 }, { recordBytes: 64 }, { facts: 2 }, { projectionCharge: 64 }, { cacheCharge: 64 }]) {
    const reader = new HistoricalUsageReader(limits);
    await expect(reader.page(f.store)).rejects.toThrow("limit");
    expect(reader.stats().cacheEntries).toBe(0);
  }
  const reader = new HistoricalUsageReader({ readBytes: 64 });
  const cancelled = new AbortController();
  const result = reader.page(f.store, 0, 1, { signal: cancelled.signal }).then(() => "unexpected pass", e => String(e));
  await whileScanning(reader);
  const other = fixture(1);
  await expect(reader.page(other.store)).rejects.toThrow("busy");
  cancelled.abort();
  expect(await result).toContain("cancelled");
  for (let i = 0; i < 100 && reader.stats().activeScans; i++) await yieldIO();
  expect(reader.stats().activeScans).toBe(0);
  expect(reader.stats().bytesRead).toBeLessThan(statSync(f.store.path).size);
  expect(reader.stats().cacheEntries).toBe(0);
  const timed = new HistoricalUsageReader({ readBytes: 16 });
  await expect(timed.page(f.store, 0, 1, { expiresAt: Date.now() + 20 })).rejects.toThrow("deadline");
  for (let i = 0; i < 100 && timed.stats().activeScans; i++) await yieldIO();
  expect(timed.stats().activeScans).toBe(0);
  expect(timed.stats().bytesRead).toBeLessThan(statSync(f.store.path).size);
});

test("concurrent callers share a cold scan and eviction requires a new declared scan", async () => {
  const a = fixture(300), b = fixture(1);
  const reader = new HistoricalUsageReader({ cacheEntries: 1 });
  const [first, second] = await Promise.all([reader.page(a.store, 0), reader.page(a.store, 128)]);
  expect(first.facts).toEqual(a.facts.slice(0, 128));
  expect(second.facts).toEqual(a.facts.slice(128, 256));
  expect(reader.stats().scans).toBe(1);
  await reader.page(b.store);
  expect(reader.stats().cacheEntries).toBe(1);
  await reader.page(a.store, 256);
  expect(reader.stats().scans).toBe(3);
  const charged = new HistoricalUsageReader({ cacheCharge: reader.stats().cacheCharge + 1 });
  await charged.page(a.store); await charged.page(b.store);
  expect(charged.stats().cacheEntries).toBe(1);
});

test("an unrelated socket responds during a cold scan, before that scan finishes", async () => {
  const f = fixture(3, 128 * 1024);
  const reader = new HistoricalUsageReader({ readBytes: 64 });
  const socket = join(f.root, "responsive.sock");
  const server = createServer(c => { c.end("ok"); });
  await new Promise<void>(resolve => server.listen(socket, resolve));
  let finished = false;
  const scan = reader.page(f.store).finally(() => { finished = true; });
  try {
    await whileScanning(reader);
    const response = await new Promise<string>((resolve, reject) => {
      const c = connect(socket); let text = "";
      c.on("data", data => { text += data.toString(); });
      c.on("end", () => resolve(text)); c.on("error", reject);
    });
    expect(response).toBe("ok");
    expect(finished).toBe(false);
    expect((await scan).facts).toEqual(f.facts);
  } finally {
    await scan;
    await new Promise<void>(resolve => server.close(() => resolve()));
  }
});

test("cold/warm measurements separate transcript bytes from fact count", async () => {
  for (const count of [10, 300]) for (const textBytes of [0, 2 * 1024 * 1024]) {
    const f = fixture(count, textBytes);
    const reader = new HistoricalUsageReader();
    const coldStart = performance.now();
    await reader.page(f.store);
    const coldMs = performance.now() - coldStart;
    const before = reader.stats();
    const warmStart = performance.now();
    for (let after = 0; after < count; after += 128) await reader.page(f.store, after);
    const warmMs = performance.now() - warmStart;
    expect(reader.stats()).toEqual(before);
    expect(before.bytesRead).toBe(statSync(f.store.path).size);
    console.log(JSON.stringify({ usage_reader_measurement: { facts: count, messageBytes: textBytes,
      sourceBytes: before.bytesRead, scanCount: before.scans, projectionCharge: before.cacheCharge, coldMs, warmMs, warmReadBytes: 0 } }));
  }
});

test("real unloaded sidecar usage route honors deadlines and keeps the seat unloaded", async () => {
  const f = fixture(300, 1024 * 1024);
  const stored = new SeatStore(join(f.root, "omp-seats"), "seat", "session");
  stored.rewrite(f.entries);
  const before = readFileSync(stored.path);
  const sock = join(f.root, "sidecar.sock");
  const proc = Bun.spawn(["bun", join(import.meta.dir, "server.ts"), sock], {
    stdout: "ignore", stderr: "pipe", env: { ...process.env, JEVONS_SPOOL_DIR: join(f.root, "spool") },
  });
  async function ask(message: object): Promise<Record<string, any>> {
    return new Promise((resolve, reject) => {
      const c = connect(sock, () => c.write(JSON.stringify(message) + "\n"));
      let text = "";
      c.setTimeout(3000, () => c.destroy(new Error("sidecar test query timed out")));
      c.on("error", reject);
      c.on("data", chunk => {
        text += chunk.toString();
        if (text.includes("\n")) { c.end(); resolve(JSON.parse(text.slice(0, text.indexOf("\n")))); }
      });
    });
  }
  try {
    // Startup probe cannot load a model or dispatch an input.
    let ready = false;
    for (let i = 0; i < 100; i++) {
      try { await ask({ op: "abort", seat: "absent" }); ready = true; break; }
      catch { await Bun.sleep(20); }
    }
    expect(ready).toBe(true);
    const expired = await ask({ op: "usage", seat: "seat", session_id: "session", expires_at: Date.now() - 1 });
    expect(expired.type).toBe("error"); expect(expired.text).toContain("deadline");
    const facts: UsageFact[] = [];
    for (let after = 0; after < f.facts.length; after += 128) {
      const result = await ask({ op: "usage", seat: "seat", session_id: "session", after, limit: 128, expires_at: Date.now() + 3000 });
      expect(result.type).toBe("usage_page");
      facts.push(...result.usage_page.facts);
    }
    expect(facts).toEqual(f.facts);
    const absent = await ask({ op: "abort", seat: "seat" });
    expect(absent.type).toBe("error"); expect(absent.text).toBe("seat is not loaded");
    expect(readFileSync(stored.path)).toEqual(before);
  } finally {
    proc.kill(); await proc.exited;
  }
});
