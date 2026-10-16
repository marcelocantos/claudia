// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { nativeUsageFixture } from "./testdata/native-usage-producer.ts";

async function fixture(scenario: string, backend = "openai") {
  const root = mkdtempSync(join(tmpdir(), "cl-native-usage-"));
  try {
    const r = await nativeUsageFixture(root, scenario, backend);
    expect(r.page.facts).toEqual(r.reopened.facts);
    expect(r.events.filter(e => e.type === "usage_error")).toEqual([]);
    expect(r.events.filter(e => e.type === "usage").map(e => e.usage_fact)).toEqual(r.page.facts);
    const finals = r.page.facts.filter(f => f.state !== "started");
    expect(r.page.facts.length).toBe(r.calls.length * 2);
    expect(new Set(finals.map(f => f.call_id)).size).toBe(r.calls.length);
    expect(r.page.facts.every(f => f.coverage === "provider_attempt" && f.call_id === f.provider_attempt_id)).toBe(true);
    if (scenario !== "cancel") {
      expect(finals.every(f => f.turn_id === "turn-A" && f.requests.length === 1 && f.requests[0].request_id === "logical-A" && f.requests[0].attempt_id === "attempt-A")).toBe(true);
    }
    return { ...r, finals, compaction: finals.filter(f => f.kind === "compaction") };
  } finally { rmSync(root, { recursive: true, force: true }); }
}

for (const backend of ["openai", "codex"]) {
  test(`${backend} native V2 compaction observes actual dispatch once and survives History rebuild`, async () => {
    const r = await fixture("success", backend);
    expect(r.calls).toEqual(["v2", "model"]);
    expect(r.compaction).toHaveLength(1);
    expect(r.compaction[0]).toMatchObject({ state: "completed", provider_response_id: "compact_v2", provider_request_id: "native-1", counters: { total_input_tokens: 100, input_tokens: 60, output_tokens: 7, cache_read_input_tokens: 40, cache_creation_input_tokens: 0, reasoning_tokens: 0 } });
    expect(r.compaction[0].invocation_id).not.toBe(r.finals[1].invocation_id);
    expect(r.events.some(e => e.type === "compacted" && e.text?.includes("method remote"))).toBe(true);
  });

  if (backend === "openai") test(`${backend} V1 fallback preserves usage on the failed V2 frame`, async () => {
    const r = await fixture("v1", backend);
    expect(r.calls).toEqual(["v2", "v1", "model"]);
    expect(r.compaction.map(f => f.state)).toEqual(["failed", "completed"]);
    expect(r.compaction[0].counters).toEqual({ total_input_tokens: 12, output_tokens: 0 });
    expect(r.compaction[1].counters).toEqual({ total_input_tokens: 20, input_tokens: 20, output_tokens: 3, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 });
    expect(r.compaction[0].invocation_id).toBe(r.compaction[1].invocation_id);
  });
}

test("native V1 without V2 has its own physical observation", async () => {
  const r = await fixture("v1only");
  expect(r.calls).toEqual(["v1", "model"]);
  expect(r.compaction[0].counters.total_input_tokens).toBe(20);
});

test("native V2 retry counts both attempts under one invocation", async () => {
  const r = await fixture("retry");
  expect(r.calls).toEqual(["v2", "v2", "model"]);
  expect(r.compaction.map(f => f.state)).toEqual(["failed", "completed"]);
  expect(r.compaction[0].counters).toEqual({ total_input_tokens: 12, output_tokens: 0 });
  expect(r.compaction[1].counters.total_input_tokens).toBe(100);
  expect(r.compaction[0].invocation_id).toBe(r.compaction[1].invocation_id);
});

test("native V2 partial usage survives the engine's stricter usage parser", async () => {
  const r = await fixture("partial");
  expect(r.calls).toEqual(["v2", "model"]);
  expect(r.compaction[0].counters).toEqual({ total_input_tokens: 12, output_tokens: 0 });
});

test("semantic missing-compaction failure does not erase measured usage", async () => {
  const r = await fixture("missing");
  expect(r.calls).toEqual(["v2", "v1", "model"]);
  expect(r.compaction[0]).toMatchObject({ state: "failed", counters: { total_input_tokens: 100, output_tokens: 7 } });
});

test("cancelled native compaction retains observed partial counters and does not fallback", async () => {
  const r = await fixture("cancel");
  expect(r.calls).toEqual(["v2"]);
  expect(r.compaction[0]).toMatchObject({ state: "cancelled", counters: { total_input_tokens: 12, output_tokens: 0 } });
});

test("native failures followed by local summaries retain every physical call without aggregate duplication", async () => {
  const r = await fixture("local");
  expect(r.calls.slice(0, 2)).toEqual(["v2", "v1"]);
  expect(r.compaction.slice(0, 2).map(f => f.state)).toEqual(["failed", "failed"]);
  expect(r.compaction[1].counters.total_input_tokens).toBe(20);
  const local = r.compaction.slice(2);
  expect(local.length).toBeGreaterThan(0);
  expect(local.every(f => f.state === "completed" && f.counters.total_input_tokens === 28)).toBe(true);
  expect(new Set(local.map(f => f.invocation_id)).size).toBe(local.length);
  expect(r.finals.at(-1)!.kind).toBe("model");
  expect(r.events.some(e => e.type === "compacted" && e.text?.includes("method soft"))).toBe(true);
});

test("Codex native failure falls directly to local summaries, without inventing a V1 request", async () => {
  const r = await fixture("local", "codex");
  expect(r.calls[0]).toBe("v2");
  expect(r.calls).not.toContain("v1");
  expect(r.compaction[0]).toMatchObject({ state: "failed", counters: { total_input_tokens: 12, output_tokens: 0 } });
  expect(r.compaction.slice(1).every(f => f.state === "completed" && f.counters.total_input_tokens === 28)).toBe(true);
  expect(r.events.some(e => e.type === "compacted" && e.text?.includes("method soft"))).toBe(true);
});

for (const fallback of [false, true]) {
  test(`native Codex WebSocket ${fallback ? "fallback" : "success"} uses the same physical observer`, async () => {
    const r = await fixture(fallback ? "websocket-fallback" : "websocket", "codex");
    expect(r.calls).toEqual(fallback ? ["ws", "v2", "model"] : ["ws", "model"]);
    expect(r.compaction.map(f => f.state)).toEqual(fallback ? ["failed", "completed"] : ["completed"]);
    expect(new Set(r.compaction.map(f => f.invocation_id)).size).toBe(1);
    expect(r.compaction.at(-1)!.counters.total_input_tokens).toBe(100);
    if (fallback) expect(r.compaction[0].counters).toEqual({ total_input_tokens: 12, output_tokens: 0 });
  });
}

test("successful native response with no raw usage retains unknown dimensions", async () => {
  const r = await fixture("absent");
  expect(r.calls).toEqual(["v2", "model"]);
  expect(r.compaction[0].state).toBe("completed");
  expect(r.compaction[0].counters).toEqual({});
});
