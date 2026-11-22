// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Analysis reproduction, 2026-11-22 (turn-delivery-protocol report, defect
// P2). Run by run_sidecar_repros.sh inside a scratch copy of sidecar/.
//
// A prompt that opens a turn on an idle seat is not yet a started request
// while the turn's pre-turn compaction runs. Release Stop (abort with
// stop_turn) ends that turn and empties the queues; then the cancelled
// compaction throws into executeTurn's catch, which puts the input back on
// the queue (seat.ts:768-774), and abortTurn's unwind runs it as an
// "interrupt-deliver" turn (seat.ts:878-903). The assertions are the
// desired state; they fail on c33e60e.
import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, launchAbortFixture, type Ev } from "./testdata/abort-fixture";

setDefaultTimeout(150_000);
const chunkText = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
const seat = "precompact-stop";
const session = "s-heavy";

function heavyStore(): string {
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
        usage: { input: 12_000, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 12_001,
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
        stopReason: "stop",
      },
    }));
  }
  return lines.join("\n") + "\n";
}

const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
afterEach(async () => {
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 4);

for (const stopTurn of [true, false]) {
  test(`an input whose turn is ${stopTurn ? "stopped (Release Stop)" : "interrupted"} during pre-turn compaction does not run`, async () => {
    const f = launchAbortFixture({
      entrypoint: join(import.meta.dir, "testdata/analysis-precompact-provider.ts"),
      readyMarker: "analysis-precompact-ready",
    });
    fixtures.push(f);
    mkdirSync(join(f.dir, "omp-seats", seat), { recursive: true });
    writeFileSync(join(f.dir, "omp-seats", seat, `${session}.jsonl`), heavyStore());
    const host = await f.ready;
    host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session });
    expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");

    host.send({ op: "prompt", seat, session_id: session, request_id: "r1", rpc_id: "rpc-r1", attempt_id: "att-r1", turn_id: "T1",
      text: "Reply with exactly: STOPPED-INPUT" });
    await host.next("T1 accepted", (e) => e.type === "accepted" && e.request_id === "r1");
    await host.next("pre-turn compaction", (e: Ev) => e.type === "compacting");
    f.child.kill("SIGUSR2"); // later compactions finish
    await Bun.sleep(200);
    host.send({ op: "abort", seat, ...(stopTurn ? { stop_turn: true } : {}) });
    const ended = await host.next("T1 ended", (e) => e.type === "turn_end" && e.turn_id === "T1");
    const ran = await host.next("input runs", (e) => e.type === "text" && String(e.text).includes("STOPPED-INPUT"), 20_000)
      .then((e) => ({ turn: e.turn_id, request: e.request_id }), () => undefined);
    await Bun.sleep(500);
    host.send({ op: "receipt", seat, session_id: session, request_id: "r1", rpc_id: "q-r1" });
    const r = await host.next("receipt", (e) => e.type === "receipt" && e.rpc_id === "q-r1");
    const turnsAfter = host.events.filter((e) => e.type === "accepted" && !e.request_id).map((e) => e.turn_id);
    console.log("ANALYSIS-P2 " + JSON.stringify({ stopTurn, endedStop: ended.stop_reason, ran, receipt: r.receipt?.state,
      receiptTurns: r.receipt?.turn_ids, unsentTurnsOpened: turnsAfter }));
    expect({ ran, receipt: r.receipt?.state }).toEqual({ ran: undefined, receipt: "cancelled" });
  });
}
