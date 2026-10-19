// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, test, setSystemTime } from "bun:test";
import { mkdtemp, mkdir, writeFile, appendFile, readFile, rm, symlink, chmod } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { GenerationDiscovery, generationLimits, type GenerationPage } from "./generations.ts";
import { legacyRuntimeDirName } from "./store.ts";

const cleanups: (() => Promise<void>)[] = [];
afterEach(async () => { setSystemTime(); for (const clean of cleanups.splice(0).reverse()) await clean(); });
async function fixture() {
  const root = await mkdtemp(join(tmpdir(), "cl-discovery-"));
  cleanups.push(() => rm(root, { recursive: true, force: true }));
  const reader = new GenerationDiscovery(root); cleanups.push(() => reader.close());
  return { root, reader };
}
async function history(root: string, seat: string, session: string) {
  const dir = join(root, seat); await mkdir(dir, { recursive: true });
  const path = join(dir, `${session}.jsonl`);
  await writeFile(path, JSON.stringify({ type: "claudia_seat", v: 1, seat, session }) + "\n");
  return path;
}
async function scan(reader: GenerationDiscovery, limit = 2) {
  const pages: GenerationPage[] = []; let cursor = "";
  do { const page = await reader.page(cursor, limit); pages.push(page); cursor = page.cursor; } while (cursor);
  return pages;
}

test("a leftover v0.54-v0.58 SDK runtime directory is not a seat and leaves the inventory complete", async () => {
  const f = await fixture();
  await history(f.root, "po", "s1");
  const runtime = join(f.root, legacyRuntimeDirName, "po");
  await mkdir(runtime, { recursive: true });
  await writeFile(join(runtime, "models.db"), "not history");
  const pages = await scan(f.reader);
  expect(pages.flatMap(p => p.generations)).toEqual([{ seat: "po", session_id: "s1" }]);
  expect(pages.flatMap(p => p.issues)).toEqual([]);
  expect(pages.at(-1)?.complete).toBe(true);
});

test("removed generations survive absent registry/spool and bounded multipage retries", async () => {
  const f = await fixture();
  for (const seat of ["removed", "active"]) for (const session of ["old", "new"]) await history(f.root, seat, session);
  const pages: GenerationPage[] = [];
  let cursor = "";
  do {
    const page = await f.reader.page(cursor, 1);
    expect(page.generations.length + page.issues.length).toBeLessThanOrEqual(1);
    if (cursor) expect(await f.reader.page(cursor, 1)).toEqual(page);
    pages.push(page); cursor = page.cursor;
  } while (cursor);
  expect(pages.flatMap(p => p.generations).map(g => `${g.seat}/${g.session_id}`).sort()).toEqual(["active/new", "active/old", "removed/new", "removed/old"]);
  expect(pages.at(-1)?.complete).toBe(true);
  expect(pages.length).toBeGreaterThan(4);
});

test("header reads are bounded independently of history length; corrupt/torn/foreign identities are issues without repair", async () => {
  const f = await fixture();
  const long = await history(f.root, "gone", "long");
  for (let i = 0; i < 16; i++) await appendFile(long, "x".repeat(1 << 20));
  const bad = await history(f.root, "gone", "bad"); await writeFile(bad, '{"type":');
  const foreign = await history(f.root, "gone", "foreign");
  await writeFile(foreign, JSON.stringify({ type: "claudia_seat", v: 1, seat: "elsewhere", session: "foreign" }) + "\n");
  const huge = await history(f.root, "gone", "huge"); await writeFile(huge, "x".repeat(generationLimits.headerBytes + 1) + "\n");
  const pages = await scan(f.reader);
  expect(pages.flatMap(p => p.generations)).toEqual([{ seat: "gone", session_id: "long" }]);
  expect(pages.flatMap(p => p.issues)).toHaveLength(3);
  expect(pages.at(-1)?.complete).toBe(false);
  expect(f.reader.bytesRead).toBeLessThanOrEqual(4 * generationLimits.headerBytes);
  expect(await readFile(bad, "utf8")).toBe('{"type":');
});

test("symlinks and unreadable files are explicit issues", async () => {
  const f = await fixture();
  const path = await history(f.root, "seat", "private");
  await chmod(path, 0);
  cleanups.push(() => chmod(path, 0o600));
  await symlink(path, join(f.root, "seat", "linked.jsonl"));
  await mkdir(join(f.root, "seat", "nested.jsonl"));
  const pages = await scan(f.reader);
  // Root can read mode-000 files; preserve the symlink/non-file assertions there.
  const privileged = process.getuid?.() === 0;
  expect(pages.flatMap(p => p.generations)).toHaveLength(privileged ? 1 : 0);
  expect(pages.flatMap(p => p.issues)).toHaveLength(privileged ? 2 : 3);
  expect(pages.at(-1)?.complete).toBe(false);
});

test("expired/restarted cursor and cancelled page require explicit restart, never skip ahead", async () => {
  const f = await fixture(); await history(f.root, "seat", "old");
  const first = await f.reader.page("", 1);
  await expect(f.reader.page(first.cursor, 1, { expiresAt: Date.now() - 1 })).rejects.toThrow("interrupted");
  // Deadline before opening the cursor has consumed no entries, so it remains usable.
  const resumed = await f.reader.page(first.cursor, 1);
  expect(resumed.generations).toEqual([{ seat: "seat", session_id: "old" }]);
  const other = new GenerationDiscovery(f.root); cleanups.push(() => other.close());
  try { await other.page(first.cursor, 1); throw new Error("accepted expired cursor"); }
  catch (err) { expect((err as any).reason).toBe("discovery_cursor_expired"); }
  const abort = new AbortController(); abort.abort();
  await expect(other.page("", 1, { signal: abort.signal })).rejects.toThrow("interrupted");
  expect((await scan(other)).at(-1)?.complete).toBe(true);
});

test("bounded scans, cursor bytes, request entries and concurrency", async () => {
  const f = await fixture(); await history(f.root, "seat", "old");
  for (let i = 0; i < generationLimits.scans; i++) await f.reader.page("", 1);
  await expect(f.reader.page("", 1)).rejects.toThrow("capacity");
  await expect(f.reader.page("x".repeat(generationLimits.cursorBytes + 1))).rejects.toThrow("invalid");
  await expect(f.reader.page("", generationLimits.page + 1)).rejects.toThrow("invalid");
  const second = new GenerationDiscovery(f.root); cleanups.push(() => second.close());
  const pending = second.page();
  await expect(second.page()).rejects.toThrow("busy"); await pending;
});

test("later sweep finds new generations, terminal error state survives pages", async () => {
  const f = await fixture(); await history(f.root, "seat", "first");
  const first = await scan(f.reader); expect(first.at(-1)?.complete).toBe(true);
  await history(f.root, "seat", "later");
  const next = await scan(f.reader);
  expect(next.flatMap(p => p.generations).map(g => g.session_id).sort()).toEqual(["first", "later"]);
});


test("idle expiry releases scans and reports typed restart, not an empty inventory", async () => {
  const f = await fixture(); await history(f.root, "seat", "old");
  const first = await f.reader.page("", 1);
  setSystemTime(new Date(Date.now() + generationLimits.ttlMS + 1));
  try { await f.reader.page(first.cursor, 1); throw new Error("accepted expired cursor"); }
  catch (err) { expect((err as any).reason).toBe("discovery_cursor_expired"); }
  expect((await scan(f.reader)).at(-1)?.complete).toBe(true);
});

test("interrupted native traversal invalidates its cursor without hiding consumed entries", async () => {
  const f = await fixture();
  for (let i = 0; i < 20; i++) await history(f.root, "seat", `generation-${i}`);
  const first = await f.reader.page("", 10);
  const controller = new AbortController();
  const pending = f.reader.page(first.cursor, 10, { signal: controller.signal });
  setImmediate(() => controller.abort());
  await expect(pending).rejects.toThrow("interrupted");
  try { await f.reader.page(first.cursor, 10); throw new Error("accepted interrupted cursor"); }
  catch (err) { expect((err as any).reason).toBe("discovery_cursor_expired"); }
  expect((await scan(f.reader, 10)).flatMap(p => p.generations)).toHaveLength(20);
});
