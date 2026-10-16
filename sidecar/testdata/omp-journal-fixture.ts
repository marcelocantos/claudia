// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import assert from "node:assert/strict";
import { appendFileSync, existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { join } from "node:path";
globalThis.fetch = (async () => { throw new Error("unexpected journal fixture network request"); }) as typeof fetch;
const { OMPJournal } = await import("../omp-journal.ts");
const { ReceiptJournal } = await import("../receipts.ts");
const { UsageJournal } = await import("../usage.ts");
const { FileSessionStorage } = await import("@oh-my-pi/pi-coding-agent/session/session-storage");
const { createMockModel } = await import("@oh-my-pi/pi-ai/providers/mock");
type Storage = InstanceType<typeof FileSessionStorage>;
type Journal = InstanceType<typeof OMPJournal>;
const root = process.argv[2];
const cwd = join(root, "work");
mkdirSync(cwd);
let sequence = 0;
const journals: Journal[] = [];
async function fresh(storage?: Storage) {
  const journal = await OMPJournal.open({ root: join(root, String(++sequence)), cwd, seat: "worker", session: "generation", storage });
  journals.push(journal);
  return journal;
}
function reopen(journal: Journal, identity = { seat: journal.store.seat, session: journal.store.session }) {
  return OMPJournal.open({ root: join(root, String(sequence)), cwd, ...identity, requireResume: true });
}
const meta = (id: string) => ({ request_id: id, session_id: "generation", attempt_id: `attempt-${id}` });
try {
  await assert.rejects(OMPJournal.open({ root: "", cwd, seat: "worker", session: "generation" }), /explicit absolute state root/);
  await assert.rejects(OMPJournal.open({ root, cwd: "", seat: "worker", session: "generation" }), /explicit absolute working directory/);
  // Accepted before any model history exists; restarting does not mint again.
  const first = await fresh();
  const receipts = new ReceiptJournal(first);
  receipts.accept("same text", meta("A"), "prompt");
  const sdkID = first.manager.getSessionId();
  assert.equal(existsSync(first.store.path), true);
  await first.close();
  const restored = await reopen(first); journals.push(restored);
  assert.equal(restored.manager.getSessionId(), sdkID);
  const recovered = new ReceiptJournal(restored);
  assert.deepEqual(recovered.pending().map(p => p.receipt.request_id), ["A"]);
  assert.equal(recovered.accept("same text", meta("A"), "prompt").fresh, false);
  assert.equal(recovered.accept("same text", meta("B"), "steer").fresh, true);
  assert.throws(() => recovered.accept("different", meta("A"), "prompt"), /conflicting payload/);
  assert.equal(restored.manager.buildSessionContext().messages.length, 0);
  recovered.start(["A"], "turn-one");
  // Independently reopen the disk snapshot at the dispatch boundary.
  const dispatchView = await reopen(restored); journals.push(dispatchView);
  assert.equal(new ReceiptJournal(dispatchView).query("A").state, "unknown");
  assert.equal(new ReceiptJournal(dispatchView).query("A").reason, "interrupted");
  await dispatchView.close();
  recovered.finish(["A"], "completed");
  await restored.close();
  const complete = await reopen(restored); journals.push(complete);
  const final = new ReceiptJournal(complete);
  assert.equal(final.query("A").state, "completed");
  assert.equal(final.accept("same text", meta("A"), "prompt").fresh, false);
  assert.deepEqual(final.pending().map(p => p.receipt.request_id), ["B"]);
  await complete.close();

  // Metadata above the pinned SDK's generic 500k string limit is lossless.
  const large = await fresh();
  const text = "a".repeat(500_001) + "🙂 original payload";
  new ReceiptJournal(large).accept(text, meta("large"), "prompt");
  assert.equal(readFileSync(large.store.path, "utf8").includes("Session persistence truncated"), false);
  await large.close();
  const largeRestored = await reopen(large); journals.push(largeRestored);
  assert.equal(new ReceiptJournal(largeRestored).pending()[0].message.content, text);
  await largeRestored.close();

  // Poison stays latched even if the SDK could later rewrite/recover its file.
  for (const failedState of ["accepted", "started", "completed", "flush", "partial"]) {
    const storage = new FileSessionStorage();
    const original = storage.openWriter.bind(storage);
    let fail = false;
    storage.openWriter = (path, options) => {
      const writer = original(path, options);
      const append = writer.appendSync!.bind(writer);
      const flush = writer.flushSync!.bind(writer);
      writer.appendSync = line => {
        const fact = JSON.parse(line);
        if (fail && fact.data?.receipt?.state === (failedState === "partial" ? "accepted" : failedState)) {
          if (failedState === "partial") appendFileSync(path, line.slice(0, 15));
          throw new Error("injected append outage");
        }
        append(line);
      };
      writer.flushSync = () => {
        if (fail && failedState === "flush") throw new Error("injected flush outage");
        flush();
      };
      return writer;
    };
    const journal = await fresh(storage);
    const r = new ReceiptJournal(journal);
    if (failedState === "started" || failedState === "completed") r.accept("input", meta("fail"), "prompt");
    if (failedState === "completed") r.start(["fail"], "turn-fail");
    fail = true;
    assert.throws(() => failedState === "started" ? r.start(["fail"], "turn-fail")
      : failedState === "completed" ? r.finish(["fail"], "completed") : r.accept("input", meta("fail"), "prompt"), /persistence failed/);
    fail = false;
    assert.throws(() => r.accept("next", meta("next"), "prompt"), /persistence failed/);
    assert.equal(r.query("fail").state, "unknown");
    if (failedState === "partial") {
      const bytes = readFileSync(journal.store.path);
      await assert.rejects(reopen(journal), /incomplete journal/);
      assert.deepEqual(readFileSync(journal.store.path), bytes);
    }
    // The SDK's own error latch is expected to make close reject for append
    // failures; a pure flush failure was injected outside that latch.
    if (failedState === "flush") await journal.close();
    else await assert.rejects(journal.close(), /injected append outage/);
  }

  for (const damage of ["middle", "tail", "binding", "parent", "transition", "usage-carrier", "binding-carrier", "duplicate-binding"]) {
    const journal = await fresh();
    const r = new ReceiptJournal(journal);
    r.accept("input", meta("corrupt"), "prompt");
    if (damage === "usage-carrier") {
      new UsageJournal(journal).begin("model", createMockModel({ handler: () => ({ content: ["ok"] }) }), { requests: [] });
    }
    await journal.close();
    let content = readFileSync(journal.store.path, "utf8");
    if (damage === "middle") content += "{not-json}\n";
    if (damage === "tail") content = content.slice(0, -1);
    if (damage === "binding") content = content.replace('"seat":"worker"', '"seat":"foreign"');
    if (damage === "parent") content = content.replace(/"parentId":null/, '"parentId":"absent"');
    if (damage === "transition") content = content.replace('"state":"accepted"', '"state":"completed"');
    if (damage === "usage-carrier") content = content.replace('"type":"custom","customType":"claudia_usage_v1"',
      '"type":"custom_message","customType":"claudia_usage_v1"');
    if (damage === "binding-carrier" || damage === "duplicate-binding") {
      content += JSON.stringify({ type: damage === "binding-carrier" ? "custom_message" : "custom",
        id: "duplicate-binding", parentId: journal.entries.at(-1)!.id, timestamp: new Date().toISOString(),
        customType: "claudia_session_v1", data: { seat: "worker", session: "generation", omp_session_id: journal.manager.getSessionId() },
        content: "metadata must not become context", display: false }) + "\n";
    }
    writeFileSync(journal.store.path, content);
    await assert.rejects(reopen(journal), /journal|binding|tree/);
    assert.equal(readFileSync(journal.store.path, "utf8"), content);
  }

  // No accepted custom fact becomes provider context, and accounting can
  // project the same journal without requiring History/Maintenance.
  const shared = await fresh();
  const usage = new UsageJournal(shared);
  const model = createMockModel({ handler: () => ({ content: ["ok"] }) });
  const started = usage.begin("model", model, { requests: [] });
  usage.finish(started);
  assert.equal(usage.page().facts.length, 2);
  assert.equal(shared.manager.buildSessionContext().messages.length, 0);
  await shared.close();
  const sharedRestored = await reopen(shared); journals.push(sharedRestored);
  assert.deepEqual(new UsageJournal(sharedRestored).page(), usage.page());
  await sharedRestored.close();

  // Independently vary prior conversation bytes while appending the same
  // number of service facts. Instrument the real SDK file backend.
  for (const priorMessages of [0, 32]) {
  const counted = new FileSessionStorage();
  let rewrites = 0, appends = 0, bytes = 0;
  const atomic = counted.writeTextAtomic.bind(counted);
  counted.writeTextAtomic = (...args) => { rewrites++; return atomic(...args); };
  const sync = counted.writeTextSync.bind(counted);
  counted.writeTextSync = (...args) => { rewrites++; return sync(...args); };
  const openWriter = counted.openWriter.bind(counted);
  counted.openWriter = (...args) => {
    const writer = openWriter(...args);
    const append = writer.appendSync!.bind(writer);
    writer.appendSync = line => { appends++; bytes += Buffer.byteLength(line); append(line); };
    return writer;
  };
  const measured = await fresh(counted);
  for (let i = 0; i < priorMessages; i++) {
    measured.manager.appendMessage({ role: "user", content: "x".repeat(128 * 1024), timestamp: Date.now() });
  }
  measured.manager.flushSync();
  rewrites = appends = bytes = 0;
  const smallSize = statSync(measured.store.path).size;
  for (let i = 0; i < 256; i++) measured.appendFact("fixture", { seq: i, value: "x".repeat(128) });
  assert.equal(rewrites, 0);
  assert.equal(appends, 256);
  assert.equal(statSync(measured.store.path).size - smallSize, bytes);
  assert.ok(bytes < 256 * 512);
  measured.manager.seal();
  assert.throws(() => measured.appendFact("fixture", { dropped: true }), /no longer accepts/);
  await measured.close();
  assert.throws(() => measured.appendFact("fixture", {}), /closed/);
  }
} finally {
  // Explicitly closed journals are idempotent, including expected-error closes.
  for (const journal of journals) await journal.close();
}
console.log("OMP journal fixture passed");
