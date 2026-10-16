// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import type { Model } from "@oh-my-pi/pi-ai";
import { History } from "./maintain.ts";
import { HistoricalUsageReader } from "./usage-reader.ts";
import { SeatStore } from "./store.ts";
import { measuredStream, measuredCompletion, UsageJournal } from "./usage.ts";
import { requestAnthropicNativeCompaction } from "@oh-my-pi/pi-agent-core/compaction/anthropic";

const roots: string[] = [];
afterEach(() => { for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true }); });
const specimen = (name: string) => readFileSync(join(import.meta.dir, "testdata/provider-usage", name), "utf8");
const refs = () => ({ turn_id: "turn-A", requests: [{ request_id: "owner-A", attempt_id: "delivery-A" }, { request_id: "owner-B", attempt_id: "delivery-B" }] });
function fixture(provider: "anthropic" | "xai-oauth") {
  const root = mkdtempSync(join(tmpdir(), "cl-provider-usage-")); roots.push(root);
  const store = new SeatStore(root, "seat", "generation");
  const history = new History(store); history.restore();
  const errors: string[] = [];
  const journal = new UsageJournal(history, () => {}, message => errors.push(message));
  const model = { ...getBundledModel(provider, provider === "anthropic" ? "claude-sonnet-4-6" : "grok-4.3")!, baseUrl: "https://fixture.invalid" } as Model;
  return { store, journal, model, errors };
}
const context = { messages: [{ role: "user" as const, content: "Fixture question", timestamp: 1 }] };
function wire(text: string, id = "http-fixture") {
  return new Response(text, { headers: { "content-type": "text/event-stream", "request-id": id, "x-request-id": id } });
}

for (const provider of ["anthropic", "xai-oauth"] as const) {
  test(`historical ${provider} pages preserve terminal provider IDs and inclusive totals`, async () => {
    const f = fixture(provider);
    await measuredStream(() => f.journal, refs)(f.model, context, {
      apiKey: "fixture", fetch: async () => wire(specimen(provider === "anthropic" ? "anthropic.sse" : "responses.sse")),
    }).result();
    expect(f.errors).toEqual([]);
    const reader = new HistoricalUsageReader();
    const started = await reader.page(f.store, 0, 1);
    expect(started.facts[0].provider_request_id).toBeUndefined();
    expect(started.facts[0].provider_response_id).toBeUndefined();
    const terminal = await reader.page(f.store, started.next, 1);
    expect(terminal.facts[0]).toMatchObject({ provider_request_id: "http-fixture",
      provider_response_id: provider === "anthropic" ? "msg_fixture" : "resp_fixture",
      counters: { total_input_tokens: 28, input_tokens: 21, cache_read_input_tokens: 7, cache_creation_input_tokens: 0, output_tokens: 4 } });
    expect([...started.facts, ...terminal.facts]).toEqual(f.journal.page().facts);
    expect(reader.stats().scans).toBe(1);
    expect(reader.stats().bytesRead).toBe(readFileSync(f.store.path).byteLength);
    const restored = new History(f.store); restored.restore();
    expect(restored.rebuild()).toEqual([]);
  });
  test(`actual ${provider} parser preserves raw zero, identities and disjoint buckets`, async () => {
    const f = fixture(provider); let calls = 0;
    const stream = measuredStream(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => { calls++; return wire(specimen(provider === "anthropic" ? "anthropic.sse" : "responses.sse")); } });
    const result = await stream.result();
    expect(result.stopReason).toBe("stop"); expect(calls).toBe(1); expect(f.errors).toEqual([]);
    const facts = f.journal.page().facts;
    expect(facts).toHaveLength(2);
    expect(facts[0]).toMatchObject({ coverage: "provider_attempt", state: "started", counters: {}, requests: refs().requests });
    expect(facts[1]).toMatchObject({ state: "completed", provider_request_id: "http-fixture", counters: { total_input_tokens: 28, input_tokens: 21, output_tokens: 4, cache_read_input_tokens: 7, cache_creation_input_tokens: 0 } });
    expect(facts[1].invocation_id).not.toBe(facts[1].provider_attempt_id);
    expect(facts[1].call_id).toBe(facts[1].provider_attempt_id);
    expect(facts[1].requests[0].attempt_id).toBe("delivery-A");
    expect(facts[1].api_equivalent_cost_usd).toBeUndefined();
    const restored = new History(f.store); restored.restore();
    expect(new UsageJournal(restored).page()).toEqual(f.journal.page());
  });
}

test("generic Responses hidden HTTP retry records failed unknown and successful attempt", async () => {
  const f = fixture("xai-oauth"); let calls = 0;
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => {
    calls++;
    return calls === 1 ? new Response('{"error":{"message":"retry fixture"}}', { status: 429, headers: { "retry-after": "0" } }) : wire(specimen("responses.sse"));
  } }).result();
  expect(result.stopReason).toBe("stop"); expect(calls).toBe(2);
  const finals = f.journal.page().facts.filter(f => f.state !== "started");
  expect(finals.map(f => f.state)).toEqual(["failed", "completed"]);
  expect(finals[0].counters).toEqual({});
  expect(new Set(finals.map(f => f.invocation_id)).size).toBe(1);
  expect(new Set(finals.map(f => f.provider_attempt_id)).size).toBe(2);
});

test("empty successful Responses retry retains the first billed response", async () => {
  const f = fixture("xai-oauth"); let calls = 0;
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => wire(specimen(++calls === 1 ? "response-empty.sse" : "responses.sse")) }).result();
  expect(result.stopReason).toBe("stop"); expect(calls).toBe(2);
  const finals = f.journal.page().facts.filter(f => f.state !== "started");
  expect(finals.map(f => f.counters.total_input_tokens)).toEqual([10, 28]);
  expect(finals.map(f => f.counters.output_tokens)).toEqual([1, 4]);
  expect(finals.every(f => f.state === "completed" && f.coverage === "provider_attempt")).toBe(true);
});

test("failed response keeps inclusive input while missing cache split stays unknown", async () => {
  const f = fixture("xai-oauth"); let calls = 0;
  await measuredStream(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => { calls++; return wire(specimen("response-failed.sse")); } }).result();
  expect(calls).toBe(1);
  const final = f.journal.page().facts.at(-1)!;
  expect(final).toMatchObject({ state: "failed", counters: { total_input_tokens: 12, output_tokens: 0 } });
  expect(final.counters.input_tokens).toBeUndefined();
  expect(final.counters.cache_read_input_tokens).toBeUndefined();
});

test("local summary completion uses the same physical observation path", async () => {
  const f = fixture("anthropic"); let calls = 0;
  await measuredCompletion(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => { calls++; return wire(specimen("anthropic.sse")); } });
  expect(calls).toBe(1);
  expect(f.journal.page().facts.at(-1)).toMatchObject({ kind: "compaction", coverage: "provider_attempt", counters: { input_tokens: 21, output_tokens: 4 } });
});

test("native Anthropic compaction traverses engine wrapper and sums iterations once", async () => {
  const f = fixture("anthropic"); let calls = 0; let body: any;
  f.model = getBundledModel("anthropic", "claude-sonnet-4-6")!;
  const result = await requestAnthropicNativeCompaction(f.model as Model<"anthropic-messages">, "fixture", {
    systemPrompt: ["Fixture"], messages: context.messages, instructions: "Summarize", maxTokens: 100,
  }, undefined, { completeImpl: measuredCompletion(() => f.journal, refs), fetch: async (_url, init) => {
    calls++; body = JSON.parse(String(init?.body)); return wire(specimen("anthropic-compaction.sse"));
  } });
  expect(calls).toBe(1); expect(result.content).toBe("Earlier work summary.");
  expect(body.context_management.edits[0].pause_after_compaction).toBe(true);
  expect(f.journal.page().facts.at(-1)).toMatchObject({ kind: "compaction", state: "completed", counters: {
    input_tokens: 103, output_tokens: 10, cache_read_input_tokens: 20, cache_creation_input_tokens: 0, total_input_tokens: 123,
  } });
});

test("Anthropic partial usage survives a later stream error and is not zero filled", async () => {
  const f = fixture("anthropic");
  const raw = specimen("anthropic.sse").split("event: message_delta")[0] + 'event: error\ndata: {"type":"error","error":{"type":"invalid_request_error","message":"fixture failure"}}\n\n';
  await measuredStream(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => wire(raw) }).result();
  const final = f.journal.page().facts.at(-1)!;
  expect(final.state).toBe("failed");
  expect(final.counters).toEqual({ input_tokens: 21, output_tokens: 0, cache_read_input_tokens: 7, cache_creation_input_tokens: 0, total_input_tokens: 28 });
});

test("invalid provider counter poisons accounting without retrying the request", async () => {
  const f = fixture("xai-oauth"); let calls = 0;
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => {
    calls++; return wire(specimen("responses.sse").replace('"input_tokens":28', '"input_tokens":-1'));
  } }).result();
  expect(result.stopReason).toBe("stop"); expect(calls).toBe(1);
  expect(f.errors).toHaveLength(1);
  expect(() => f.journal.page()).toThrow(/invalid counter/);
});

test("Anthropic cumulative input correction recomputes inclusive input", async () => {
  const f = fixture("anthropic");
  await measuredStream(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => wire(
    specimen("anthropic.sse").replace('"usage":{"output_tokens":4}', '"usage":{"input_tokens":30,"output_tokens":4}'),
  ) }).result();
  expect(f.journal.page().facts.at(-1)!.counters).toMatchObject({ total_input_tokens: 37, input_tokens: 30, cache_read_input_tokens: 7 });
});

test("cancelled Anthropic response preserves already observed raw input", async () => {
  const f = fixture("anthropic"); const abort = new AbortController(); let calls = 0;
  const result = await measuredStream(() => f.journal, refs)(f.model, context, {
    apiKey: "fixture", signal: abort.signal,
    onSseEvent: event => { if (event.event === "content_block_start") abort.abort(); },
    fetch: async () => { calls++; return wire(specimen("anthropic.sse")); },
  }).result();
  expect(result.stopReason).toBe("aborted"); expect(calls).toBe(1);
  const final = f.journal.page().facts.at(-1)!;
  expect(final.state).toBe("cancelled");
  expect(final.counters.input_tokens).toBe(21);
});

test("physical accounting write failure surfaces without another provider attempt", async () => {
  const f = fixture("anthropic"); let calls = 0;
  f.store.append = () => { throw new Error("fixture journal failure"); };
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { apiKey: "fixture", fetch: async () => {
    calls++; return wire(specimen("anthropic.sse"));
  } }).result();
  expect(result.stopReason).toBe("stop"); expect(calls).toBe(1);
  expect(f.errors).toHaveLength(1);
  expect(() => f.journal.page()).toThrow(/journal failure/);
});
