// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// C5: a prompt refused while the seat is compacting must not leave a durable
// "accepted" obligation. The host was told the turn failed.
import { afterEach, describe, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, writeFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, Host, launchAbortFixture, type Ev } from "./testdata/abort-fixture";

setDefaultTimeout(90_000);
const chunkText = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
const seat = "refused-prompt";
const session = "s-heavy";

function heavyStoreLines(): string[] {
  const lines = [JSON.stringify({ type: "claudia_seat", v: 1, seat, session })];
  for (let i = 0; i < 16; i++) {
    lines.push(JSON.stringify({
      type: "message", id: `u${i}`, parentId: i === 0 ? null : `a${i - 1}`,
      timestamp: new Date(Date.UTC(2026, 9, 31, 0, 0, i)).toISOString(),
      message: { role: "user", content: `${i}: ${chunkText}`, timestamp: i * 2 },
    }));
    lines.push(JSON.stringify({
      type: "message", id: `a${i}`, parentId: `u${i}`,
      timestamp: new Date(Date.UTC(2026, 9, 31, 0, 0, i, 500)).toISOString(),
      message: {
        role: "assistant", content: [{ type: "text", text: "ok" }], api: "anthropic-messages",
        provider: "anthropic", model: "claude-sonnet-5", timestamp: i * 2 + 1,
        usage: {
          input: 12_000, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 12_001,
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
        },
        stopReason: "stop",
      },
    }));
  }
  return lines;
}

function storePath(root: string) { return join(root, "omp-seats", seat, `${session}.jsonl`); }
function writeStore(root: string, body: string) {
  mkdirSync(join(root, "omp-seats", seat), { recursive: true });
  writeFileSync(storePath(root), body);
}

function prompt(request: string, turn: string, text: string) {
  return {
    op: "prompt", seat, session_id: session, request_id: request,
    rpc_id: `rpc-${request}-${turn}`, attempt_id: `att-${request}`, turn_id: turn, text,
  };
}

const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
function launch() {
  const f = launchAbortFixture({
    entrypoint: join(import.meta.dir, "testdata/t244-interleave-provider.ts"),
    readyMarker: "t244-interleave-fixture-ready",
  });
  fixtures.push(f);
  return f;
}
afterEach(async () => {
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 4);

async function load(host: Host, cwd: string) {
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd, session_id: session });
  const ready = await host.next("ready", (e) => e.type === "ready" || e.type === "error");
  expect(ready.type).toBe("ready");
}

// Refuse a prompt (request r-slip) while a bound compaction is held, then
// cancel the compaction so the seat is idle again.
async function refuseDuringCompaction(host: Host) {
  host.send({ op: "compact", seat, session_id: session, request_id: "c-hold", bound_tokens: 180_000 });
  await host.next("compacting", (e: Ev) => e.type === "compacting");
  host.send(prompt("r-slip", "T1", "refused owner message"));
  const err = await host.next("prompt refused", (e) => e.type === "error" && e.request_id === "r-slip");
  expect(err.reason).toBe("turn_in_flight");
  host.send({ op: "compact_abort", seat, request_id: "c-hold" });
  await host.next("compact settled", (e) => e.type === "error" && e.request_id === "c-hold");
}

describe("prompt refused during compaction (C5)", () => {
  test("the refusal leaves no accepted receipt behind", async () => {
    const f = launch();
    writeStore(f.dir, heavyStoreLines().join("\n") + "\n");
    const host = await f.ready;
    await load(host, f.dir);
    await refuseDuringCompaction(host);
    host.send({ op: "receipt", seat, session_id: session, request_id: "r-slip", rpc_id: "q-slip" });
    const r = await host.next("receipt", (e) => e.type === "receipt" && e.rpc_id === "q-slip");
    expect(r.receipt.state).not.toBe("accepted");
  });

  test("an ack retry of the refused request runs it", async () => {
    const f = launch();
    writeStore(f.dir, heavyStoreLines().join("\n") + "\n");
    const host = await f.ready;
    await load(host, f.dir);
    await refuseDuringCompaction(host);
    host.send(prompt("r-slip", "T2", "refused owner message"));
    const accepted = await host.next("accepted", (e) => e.type === "accepted" && e.request_id === "r-slip");
    expect(accepted.receipt.state).toBe("accepted");
    const ended = await host.next("turn end", (e) => e.type === "turn_end" || (e.type === "error" && e.request_id !== "c-hold" && e.request_id !== "r-slip"), 10_000)
      .catch((err) => ({ type: "timeout", text: String(err) } as Ev));
    expect(ended.type).toBe("turn_end");
  });

  test("a sidecar restart does not run the refused prompt", async () => {
    const f1 = launch();
    writeStore(f1.dir, heavyStoreLines().join("\n") + "\n");
    const h1 = await f1.ready;
    await load(h1, f1.dir);
    await refuseDuringCompaction(h1);
    const durable = readFileSync(storePath(f1.dir), "utf8");
    await f1.close();
    fixtures.splice(fixtures.indexOf(f1), 1);

    const f2 = launch();
    writeStore(f2.dir, durable);
    const h2 = await f2.ready;
    await load(h2, f2.dir);
    h2.send(prompt("r-new", "T3", "a fresh owner message"));
    await h2.next("turn end", (e) => e.type === "turn_end" && e.turn_id === "T3", 15_000).catch(() => undefined);
    const absorbed = h2.events.filter((e) => (e.type === "absorbed" || e.type === "text") && String(e.text).includes("refused owner"));
    expect(absorbed.map((e) => [e.type, e.text, e.request_id])).toEqual([]);
  });

  test("a prompt refused during a switch-time fold leaves no accepted receipt", async () => {
    const f = launch();
    writeStore(f.dir, heavyStoreLines().join("\n") + "\n");
    const host = await f.ready;
    await load(host, f.dir);
    host.send({ op: "load", seat, provider: "anthropic", model: "claude-haiku-4-5", token: "fixture", cwd: f.dir, session_id: session, request_id: "switch-1" });
    await host.next("compacting", (e: Ev) => e.type === "compacting");
    host.send(prompt("r-slip", "T1", "refused owner message"));
    const err = await host.next("prompt refused", (e) => e.type === "error" && e.request_id === "r-slip");
    expect(err.reason).toBe("turn_in_flight");
    host.send({ op: "abort", seat });
    const sw = await host.next("switch settled", (e) => (e.type === "error" || e.type === "ready") && e.request_id === "switch-1");
    expect(sw.type).toBe("error");
    host.send({ op: "receipt", seat, session_id: session, request_id: "r-slip", rpc_id: "q-slip" });
    const r = await host.next("receipt", (e) => e.type === "receipt" && e.rpc_id === "q-slip");
    expect(r.receipt.state).not.toBe("accepted");
  });
});
