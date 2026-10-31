// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// 🎯T244 clause 2: adversarial production server.ts wire oracles for compact
// versus prompt/steer after compact-only withSeatCommand chaining.
import { afterEach, beforeEach, describe, expect, setDefaultTimeout, test } from "bun:test";
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";
import { ABORT_SEMANTIC_MS, FIXTURE_CLEANUP_MS, FIXTURE_STARTUP_MS, Host, launchAbortFixture, type Ev } from "./testdata/abort-fixture";

setDefaultTimeout(ABORT_SEMANTIC_MS);
const bound180k = 180_000;
const chunkText = "the quick brown fox jumps over the lazy dog. ".repeat(1200);

function prompt(seat: string, session: string, request: string, turn: string, text: string) {
  return { op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text };
}

function steer(seat: string, session: string, request: string, turn: string, text: string) {
  return { op: "steer", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text };
}

function writeHeavyStore(root: string, seat: string, session: string) {
  const dir = join(root, "omp-seats", seat);
  mkdirSync(dir, { recursive: true });
  const path = join(dir, `${session}.jsonl`);
  const lines: string[] = [
    JSON.stringify({ type: "claudia_seat", v: 1, seat, session }),
  ];
  for (let i = 0; i < 16; i++) {
    lines.push(
      JSON.stringify({
        type: "message",
        id: `u${i}`,
        parentId: i === 0 ? null : `a${i - 1}`,
        timestamp: new Date(Date.UTC(2026, 9, 31, 0, 0, i)).toISOString(),
        message: { role: "user", content: `${i}: ${chunkText}`, timestamp: i * 2 },
      } as SessionEntry),
    );
    lines.push(
      JSON.stringify({
        type: "message",
        id: `a${i}`,
        parentId: `u${i}`,
        timestamp: new Date(Date.UTC(2026, 9, 31, 0, 0, i, 500)).toISOString(),
        message: {
          role: "assistant",
          content: [{ type: "text", text: "ok" }],
          api: "anthropic-messages",
          provider: "anthropic",
          model: "claude-sonnet-5",
          timestamp: i * 2 + 1,
          usage: { input: 12_000, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 12_001, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
          stopReason: "stop",
        },
      } as SessionEntry),
    );
  }
  writeFileSync(path, lines.join("\n") + "\n");
}

function countCompactionEntries(root: string, seat: string, session: string): number {
  const path = join(root, "omp-seats", seat, `${session}.jsonl`);
  try {
    return readFileSync(path, "utf8").split("\n").filter((l) => l.includes('"type":"compaction"')).length;
  } catch {
    return 0;
  }
}

async function loadSeat(host: Host, dir: string, seat: string, session: string) {
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: dir, session_id: session });
  const ready = await host.next("ready", (e) => e.type === "ready" || e.type === "error");
  expect(ready.type).toBe("ready");
}

describe("server compact interleave (🎯T244 clause 2)", () => {
  let fixture: ReturnType<typeof launchAbortFixture>;
  let host: Host;

  beforeEach(async () => {
    fixture = launchAbortFixture({
      entrypoint: join(import.meta.dir, "testdata/t244-interleave-provider.ts"),
      readyMarker: "t244-interleave-fixture-ready",
    });
    host = await fixture.ready;
  }, FIXTURE_STARTUP_MS + FIXTURE_CLEANUP_MS);
  afterEach(async () => {
    await fixture?.close();
  }, FIXTURE_CLEANUP_MS);

  test("compact during an in-flight prompt is refused before mutation", async () => {
    const { dir } = fixture;
    const seat = "race-compact";
    const session = "s1";
    await loadSeat(host, dir, seat, session);
    const before = countCompactionEntries(dir, seat, session);
    host.send(prompt(seat, session, "r-hold", "T1", "HOLD open turn"));
    await host.next("started", (e) => e.type === "receipt" && e.request_id === "r-hold" && e.receipt?.state === "started");
    host.send({ op: "compact", seat, session_id: session, request_id: "c1", bound_tokens: bound180k });
    const err = await host.next("compact refused", (e) => e.type === "error" && e.request_id === "c1");
    expect(err.reason).toBe("turn_in_flight");
    expect(countCompactionEntries(dir, seat, session)).toBe(before);
    host.send({ op: "abort", seat });
    await host.next("aborted", (e) => e.type === "turn_end" || e.type === "aborted");
    host.close();
  });

  test("prompt during held bound compact is refused before mutation", async () => {
    const { dir } = fixture;
    const seat = "race-prompt";
    const session = "s-heavy";
    writeHeavyStore(dir, seat, session);
    await loadSeat(host, dir, seat, session);
    const before = countCompactionEntries(dir, seat, session);
    const compacting = host.next("compacting", (e: Ev) => e.type === "compacting").catch(() => undefined);
    host.send({ op: "compact", seat, session_id: session, request_id: "c-hold", bound_tokens: bound180k });
    await compacting;
    host.send(prompt(seat, session, "r-slip", "T9", "NEXT LARGE owner message must not slip"));
    const err = await host.next("prompt refused", (e) => e.type === "error" && e.request_id === "r-slip");
    expect(err.reason).toBe("turn_in_flight");
    expect(countCompactionEntries(dir, seat, session)).toBe(before);
    host.send({ op: "compact_abort", seat, request_id: "c-hold" });
    await host.next("compact abort ack", (e) => e.type === "compact_abort_ack" && e.request_id === "c-hold");
    const compactErr = await host.next("compact canceled", (e) => e.type === "error" && e.request_id === "c-hold");
    expect(compactErr.reason).toMatch(/compact_canceled|turn_in_flight/);
    host.close();
  });

  test("steer during held bound compact is refused before mutation", async () => {
    const { dir } = fixture;
    const seat = "race-steer";
    const session = "s-heavy";
    writeHeavyStore(dir, seat, session);
    await loadSeat(host, dir, seat, session);
    host.send({ op: "compact", seat, session_id: session, request_id: "c2", bound_tokens: bound180k });
    await host.next("compacting", (e) => e.type === "compacting");
    host.send(steer(seat, session, "s-slip", "T8", "steer must not interleave"));
    const err = await host.next("steer refused", (e) => e.type === "error" && e.request_id === "s-slip");
    expect(err.reason).toBe("turn_in_flight");
    host.send({ op: "compact_abort", seat, request_id: "c2" });
    await host.next("abort ack", (e) => e.type === "compact_abort_ack");
    host.close();
  });
});
