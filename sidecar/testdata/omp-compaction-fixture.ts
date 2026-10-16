// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import assert from "node:assert/strict";
import { mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import type { AgentMessage } from "@oh-my-pi/pi-agent-core";

// No fallback to a real transport: native and local routes each receive the
// explicit fake fetch below, after all SDK state paths were isolated by parent.
globalThis.fetch = (async () => { throw new Error("unexpected global network request"); }) as typeof fetch;
const { Agent } = await import("@oh-my-pi/pi-agent-core");
const { type } = await import("@oh-my-pi/omptype");
const { getBundledModel } = await import("@oh-my-pi/pi-catalog/models");
const { createOmpSession, convertOmpMessages } = await import("../omp-session.ts");
const { OMPJournal } = await import("../omp-journal.ts");
const { UsageJournal, measuredStream, observedNativeCompaction } = await import("../usage.ts");
const [root, backend, scenario] = process.argv.slice(2);
const automatic = scenario.startsWith("auto-");
const mid = scenario.startsWith("mid-");
const local = scenario.endsWith("local");
const cwd = join(root, "work");
mkdirSync(cwd);
const model = { ...getBundledModel(backend === "codex" ? "openai-codex" : "openai", "gpt-5.5")!,
  baseUrl: "https://fixture.invalid", preferWebsockets: false, contextWindow: 100_000,
  remoteCompaction: { enabled: true, v2StreamingEnabled: true } };
const journalOptions = { root: join(root, "sessions"), seat: "compaction-seat", session: "compaction-generation", cwd };
const journal = await OMPJournal.open(journalOptions);
// Mid-turn history must stay under the pre-prompt threshold. The continuing
// tool call reports the occupancy that crosses it. A 90k seed would compact
// before the first provider request and never reach maintainContextMidRun.
const seedCount = mid ? 4 : 7;
const seedWords = mid ? 800 : 6000;
const seedUsage = mid
  ? { input: 100, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 101, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }
  : { input: 90_000, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 90_001, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
for (let i = 0; i < seedCount; i++) {
  journal.manager.appendMessage({ role: "user", content: `Old turn ${i}: ${"word ".repeat(seedWords)}`, timestamp: 1_000 + i * 2 });
  journal.manager.appendMessage({ role: "assistant", content: [{ type: "text", text: "Old answer" }],
    api: model.api, provider: model.provider, model: model.id,
    usage: { ...seedUsage, cost: { ...seedUsage.cost } }, stopReason: "stop", timestamp: 1_001 + i * 2 } as AgentMessage);
}
await journal.flush();
const errors: string[] = [];
const usage = new UsageJournal(journal, () => {}, error => errors.push(error));
// Attribution is the host's open turn, read when each observer is built.
// It is not a fixture constant and it is not captured at session construction.
const turn = (suffix: string) => ({
  turn_id: `turn-${backend}-${scenario}${suffix}`,
  request_id: `request-${backend}-${scenario}${suffix}`,
  attempt_id: `attempt-${backend}-${scenario}${suffix}`,
});
const opened = turn("");
const followed = turn("-followed");
let active = opened;
const attribution = () => ({
  turn_id: active.turn_id,
  requests: [{ request_id: active.request_id, attempt_id: active.attempt_id }],
});
const calls: { route: string; model: string; kind: string }[] = [];
const authorizations: string[] = [];
const factoryModels: string[] = [];
let activeKind = "model";
// The agent callback is deliberately stale. Dispatch must follow tokenFor.
let brokerToken: string | undefined = "fixture-token";
const fakeFetch = (async (input: string | URL | Request, init?: RequestInit) => {
  const url = input instanceof Request ? input.url : String(input);
  if (!url.startsWith("https://fixture.invalid/")) throw new Error(`unexpected fixture URL ${url}`);
  authorizations.push(new Headers(init?.headers).get("authorization") ?? "");
  const body = JSON.parse(String(init?.body));
  const route = url.endsWith("/compact") ? "v1" : body.input?.some((item: { type?: string }) => item.type === "compaction_trigger") ? "v2" : "summary";
  calls.push({ route, model: body.model, kind: route === "summary" ? activeKind : "compaction" });
  if (calls.length > 24) throw new Error("unexpected extra fixture request");
  const toolCall = mid && route === "summary" && activeKind === "model" && toolTurn;
  if (toolCall) toolTurn = false;
  const file = route === "v1" ? local ? "compact-v1-missing.json" : "compact-v1.json"
    : route === "v2" ? local ? "compact-failed.sse" : scenario.endsWith("absent") ? "compact-absent.sse" : "compact-v2.sse"
    : "responses.sse";
  const payload = toolCall ? toolCallSse : readFileSync(join(import.meta.dir, "provider-usage", file), "utf8");
  return new Response(payload, {
    headers: { "content-type": route === "v1" ? "application/json" : "text/event-stream", "x-request-id": `sdk-${calls.length}` },
  });
}) as typeof fetch;
const measuredModel = measuredStream(() => usage, attribution);
const measuredSide = measuredStream(() => usage, attribution, "compaction");
const fixtureTool = {
  name: "fixture_tool", label: "fixture", description: "Continues the current provider turn.",
  parameters: type({}), intent: "omit" as const,
  execute: async () => ({ content: [{ type: "text" as const, text: "tool ok" }] }),
};
let toolTurn = false;
const toolCallSse = [
  'event: response.created',
  'data: {"type":"response.created","response":{"id":"resp_tool","status":"in_progress","output":[]}}',
  '',
  'event: response.output_item.added',
  'data: {"type":"response.output_item.added","output_index":0,"item":{"id":"item_tool","type":"function_call","call_id":"call_fixture","name":"fixture_tool","arguments":"{}","status":"in_progress"}}',
  '',
  'event: response.output_item.done',
  'data: {"type":"response.output_item.done","output_index":0,"item":{"id":"item_tool","type":"function_call","call_id":"call_fixture","name":"fixture_tool","arguments":"{}","status":"completed"}}',
  '',
  'event: response.completed',
  'data: {"type":"response.completed","response":{"id":"resp_tool","status":"completed","output":[],"usage":{"input_tokens":90000,"output_tokens":1,"total_tokens":90001,"output_tokens_details":{"reasoning_tokens":0}}}}',
  '',
].join("\n");
const agent = new Agent({ initialState: { model, tools: mid ? [fixtureTool] : [], systemPrompt: ["Reply OK."] }, cwd,
  getApiKey: () => "stale-agent-token", convertToLlm: convertOmpMessages,
  streamFn: (m, c, o) => { activeKind = "model"; return measuredModel(m, c, { ...o, fetch: fakeFetch }); },
});
const adapter = await createOmpSession({ agent, sessionManager: journal.manager, stateDir: root,
  tokenFor: provider => provider === model.provider ? brokerToken : undefined,
  sideStreamFn: (m, c, o) => { activeKind = "compaction"; return measuredSide(m, c, { ...o, fetch: fakeFetch }); },
  nativeCompactionOptions: candidate => {
    factoryModels.push(candidate.id);
    return observedNativeCompaction(usage, attribution, fakeFetch)(candidate);
  },
});
// Remote first, then the local summary. The adapter must not add a bundled
// sibling model: that call would leave the host's fixture transport.
adapter.settings.set("compaction.methodOrder", ["remote", "soft"]);
adapter.settings.set("compaction.keepRecentTokens", 1000);
// The next threshold check counts the live transcript and compacts the session
// branch. Both copies have to carry the oversized turn.
const rememberBulk = (label: string) => {
  for (let i = 0; i < 7; i++) {
    const user = { role: "user" as const, content: `${label} ${i}: ${"word ".repeat(6000)}`, timestamp: Date.now() };
    const assistant = { role: "assistant" as const, content: [{ type: "text" as const, text: "Old answer" }],
      api: model.api, provider: model.provider, model: model.id,
      usage: { input: 90_000, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 90_001,
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
      stopReason: "stop" as const, timestamp: Date.now() } as AgentMessage;
    agent.appendMessage(user);
    agent.appendMessage(assistant);
    journal.manager.appendMessage(user);
    journal.manager.appendMessage(assistant);
  }
};
// Same cut-point material as the opening seed, still under the pre-prompt
// threshold, so the second turn also compacts from inside the tool loop.
const rememberSeed = (label: string) => {
  for (let i = 0; i < seedCount; i++) {
    const user = { role: "user" as const, content: `${label} ${i}: ${"word ".repeat(seedWords)}`, timestamp: Date.now() };
    const assistant = { role: "assistant" as const, content: [{ type: "text" as const, text: "Old answer" }],
      api: model.api, provider: model.provider, model: model.id,
      usage: { ...seedUsage, cost: { ...seedUsage.cost } }, stopReason: "stop" as const, timestamp: Date.now() } as AgentMessage;
    agent.appendMessage(user);
    agent.appendMessage(assistant);
    journal.manager.appendMessage(user);
    journal.manager.appendMessage(assistant);
  }
};
const promptAs = (turn: typeof opened) => {
  active = turn;
  if (mid) toolTurn = true;
  return adapter.session.promptCustomMessage({ customType: "claudia-request", content: "Continue.", display: true,
    attribution: "user", details: { request_id: turn.request_id, attempt_id: turn.attempt_id } });
};
try {
  let followAt = calls.length;
  if (scenario === "refresh") {
    await adapter.session.compact(undefined, { suppressContinuation: true });
    followAt = calls.length;
    const first = authorizations.splice(0, authorizations.length);
    assert.ok(first.length > 0);
    assert.ok(first.every(header => header === "Bearer fixture-token"));
    brokerToken = "fixture-token-refreshed";
    rememberBulk("refreshed");
    await promptAs(followed);
    const second = authorizations.splice(0, authorizations.length);
    assert.ok(second.length > 0);
    assert.ok(second.every(header => header === "Bearer fixture-token-refreshed"));
    assert.equal(second.some(header => header.includes("stale-agent-token")), false);
    const dispatched = calls.length;
    brokerToken = undefined;
    await assert.rejects(promptAs(opened), /No API key found/);
    assert.equal(calls.length, dispatched);
    assert.equal(authorizations.length, 0);
  } else if (automatic) {
    await promptAs(opened);
    followAt = calls.length;
    rememberBulk("followed");
    await promptAs(followed);
  } else if (mid) {
    await promptAs(opened);
    followAt = calls.length;
    rememberSeed("followed");
    await promptAs(followed);
  } else {
    await adapter.session.compact(undefined, { suppressContinuation: true });
  }
  const page = usage.page();
  const finals = page.facts.filter(fact => fact.state !== "started");
  assert.deepEqual(errors, []);
  assert.equal(page.facts.length, calls.length * 2);
  assert.equal(finals.length, calls.length);
  assert.equal(new Set(finals.map(fact => fact.call_id)).size, calls.length);
  assert.ok(finals.every(fact => fact.coverage === "provider_attempt" && fact.call_id === fact.provider_attempt_id && fact.model === model.id));
  assert.deepEqual(finals.map(fact => fact.kind), calls.map(call => call.kind));
  assert.notEqual(opened.turn_id, followed.turn_id);
  for (const [index, fact] of finals.entries()) {
    const turn = (automatic || scenario === "refresh" || mid) && index >= followAt ? followed : opened;
    assert.equal(fact.turn_id, turn.turn_id);
    assert.deepEqual(fact.requests, [{ request_id: turn.request_id, attempt_id: turn.attempt_id }]);
  }
  assert.ok(factoryModels.every(id => id === model.id));
  const split = automatic || scenario === "refresh" || mid;
  const firstCalls = calls.slice(0, split ? followAt : calls.length);
  const firstFinals = finals.slice(0, split ? followAt : finals.length);
  const followCalls = split ? calls.slice(followAt) : [];
  const followFinals = split ? finals.slice(followAt) : [];
  const account = (sliceCalls: typeof calls, sliceFacts: typeof finals) => {
    const paired = sliceFacts.map((fact, index) => ({ fact, call: sliceCalls[index] }));
    const native = paired.filter(item => item.call.route === "v2" || item.call.route === "v1");
    assert.ok(native.length > 0);
    assert.equal(new Set(native.map(item => item.fact.invocation_id)).size, 1);
    const summaries = paired.filter(item => item.call.route === "summary" && item.fact.kind === "compaction");
    assert.equal(new Set(summaries.map(item => item.fact.invocation_id)).size, summaries.length);
    assert.ok(summaries.every(item => item.fact.invocation_id !== native[0].fact.invocation_id));
    return paired;
  };
  account(firstCalls, firstFinals);
  if (split) {
    assert.ok(followAt > 0 && followCalls.length > 0);
    const followNative = account(followCalls, followFinals).filter(item => item.call.route === "v2" || item.call.route === "v1");
    assert.notEqual(followNative[0].fact.invocation_id, firstFinals.find((_, index) => firstCalls[index].route === "v2")?.invocation_id);
    if (mid) {
      // A pre-prompt pass would have put native compaction first. The second
      // turn has to cross the threshold inside its own tool loop too.
      assert.equal(followCalls[0].route, "summary");
      assert.equal(followFinals[0].kind, "model");
      assert.equal(followFinals[0].counters.total_input_tokens, 90_000);
      assert.ok(followCalls.some(call => call.route === "v2"));
    }
  }
  const routes = firstCalls.map(call => call.route);
  const summaries = firstFinals.filter((fact, index) => firstCalls[index].route === "summary" && fact.kind === "compaction");
  const modelTurns = firstFinals.filter(fact => fact.kind === "model");
  const nativeStart = mid ? 1 : 0;
  const nativeCount = backend === "codex" ? 1 : 2;
  if (local) {
    assert.deepEqual(routes.slice(nativeStart, nativeStart + nativeCount), backend === "codex" ? ["v2"] : ["v2", "v1"]);
    assert.ok(!routes.includes("v1") || backend !== "codex");
    assert.deepEqual(firstFinals.slice(nativeStart, nativeStart + nativeCount).map(fact => fact.state), backend === "codex" ? ["failed"] : ["failed", "failed"]);
    if (backend !== "codex") assert.equal(firstFinals[nativeStart + 1].counters.total_input_tokens, 20);
    else assert.deepEqual(firstFinals[nativeStart].counters, { total_input_tokens: 12, output_tokens: 0 });
    assert.ok(summaries.length > 0);
    assert.ok(summaries.every(fact => fact.state === "completed" && fact.counters.total_input_tokens === 28));
    if (mid) {
      assert.equal(routes[0], "summary");
      assert.equal(firstFinals[0].kind, "model");
      assert.equal(firstFinals[0].counters.total_input_tokens, 90_000);
      assert.equal(routes.at(-1), "summary");
      assert.equal(firstFinals.at(-1)!.kind, "model");
    }
  } else if (scenario === "absent") {
    assert.deepEqual(routes, ["v2"]);
    assert.equal(firstFinals[0].state, "completed");
    assert.deepEqual(firstFinals[0].counters, {});
  } else if (mid) {
    assert.deepEqual(routes.slice(0, 2), ["summary", "v2"]);
    assert.equal(firstFinals[0].kind, "model");
    assert.equal(firstFinals[0].counters.total_input_tokens, 90_000);
    assert.equal(firstFinals[1].state, "completed");
    if (scenario === "mid-absent") assert.deepEqual(firstFinals[1].counters, {});
    else {
      assert.deepEqual(firstFinals[1].counters, { total_input_tokens: 100, input_tokens: 60, output_tokens: 7, cache_read_input_tokens: 40, cache_creation_input_tokens: 0, reasoning_tokens: 0 });
      assert.equal(firstFinals[1].provider_response_id, "compact_v2");
    }
    assert.equal(routes.at(-1), "summary");
    assert.equal(firstFinals.at(-1)!.kind, "model");
  } else {
    assert.deepEqual(routes, automatic ? ["v2", "summary"] : ["v2"]);
    assert.equal(firstFinals[0].state, "completed");
    assert.deepEqual(firstFinals[0].counters, { total_input_tokens: 100, input_tokens: 60, output_tokens: 7, cache_read_input_tokens: 40, cache_creation_input_tokens: 0, reasoning_tokens: 0 });
    assert.equal(firstFinals[0].provider_response_id, "compact_v2");
  }
  assert.equal(modelTurns.length, mid ? 2 : automatic ? 1 : 0);
  if (automatic) assert.equal(modelTurns[0].counters.total_input_tokens, 28);
  if (mid) assert.equal(modelTurns[1].counters.total_input_tokens, 28);
  await journal.flush();
  await adapter.dispose();
  await journal.close();
  const reopened = await OMPJournal.open({ ...journalOptions, requireResume: true });
  try { assert.deepEqual(new UsageJournal(reopened).page(), page); }
  finally { await reopened.close(); }
  console.log(JSON.stringify({ backend, scenario, verified: true, calls, facts: finals.length }));
} finally {
  await adapter.dispose();
  await journal.close();
}
