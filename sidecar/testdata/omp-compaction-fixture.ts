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
const { getBundledModel } = await import("@oh-my-pi/pi-catalog/models");
const { createOmpSession, convertOmpMessages } = await import("../omp-session.ts");
const { OMPJournal } = await import("../omp-journal.ts");
const { UsageJournal, measuredStream } = await import("../usage.ts");
const [root, backend, scenario] = process.argv.slice(2);
const automatic = scenario.startsWith("auto-");
const local = scenario.endsWith("local");
const cwd = join(root, "work");
mkdirSync(cwd);
const model = { ...getBundledModel(backend === "codex" ? "openai-codex" : "openai", "gpt-5.5")!,
  baseUrl: "https://fixture.invalid", preferWebsockets: false, contextWindow: 100_000,
  remoteCompaction: { enabled: true, v2StreamingEnabled: true } };
const journalOptions = { root: join(root, "sessions"), seat: "compaction-seat", session: "compaction-generation", cwd };
const journal = await OMPJournal.open(journalOptions);
for (let i = 0; i < 7; i++) {
  journal.manager.appendMessage({ role: "user", content: `Old turn ${i}: ${"word ".repeat(6000)}`, timestamp: 1_000 + i * 2 });
  journal.manager.appendMessage({ role: "assistant", content: [{ type: "text", text: "Old answer" }],
    api: model.api, provider: model.provider, model: model.id,
    usage: { input: 90_000, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 90_001,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
    stopReason: "stop", timestamp: 1_001 + i * 2 } as AgentMessage);
}
await journal.flush();
const errors: string[] = [];
const usage = new UsageJournal(journal, () => {}, error => errors.push(error));
const attribution = () => ({ turn_id: "turn-A", requests: [{ request_id: "logical-A", attempt_id: "attempt-A" }] });
const calls: { route: string; model: string; kind: string }[] = [];
const factoryModels: string[] = [];
let activeKind = "model";
const fakeFetch = (async (input: string | URL | Request, init?: RequestInit) => {
  const url = input instanceof Request ? input.url : String(input);
  if (!url.startsWith("https://fixture.invalid/")) throw new Error(`unexpected fixture URL ${url}`);
  const body = JSON.parse(String(init?.body));
  const route = url.endsWith("/compact") ? "v1" : body.input?.some((item: { type?: string }) => item.type === "compaction_trigger") ? "v2" : "summary";
  calls.push({ route, model: body.model, kind: route === "summary" ? activeKind : "compaction" });
  if (calls.length > 12) throw new Error("unexpected extra fixture request");
  const file = route === "v1" ? local ? "compact-v1-missing.json" : "compact-v1.json"
    : route === "v2" ? local ? "compact-failed.sse" : scenario === "absent" ? "compact-absent.sse" : "compact-v2.sse"
    : "responses.sse";
  return new Response(readFileSync(join(import.meta.dir, "provider-usage", file), "utf8"), {
    headers: { "content-type": route === "v1" ? "application/json" : "text/event-stream", "x-request-id": `sdk-${calls.length}` },
  });
}) as typeof fetch;
const measuredModel = measuredStream(() => usage, attribution);
const measuredSide = measuredStream(() => usage, attribution, "compaction");
const agent = new Agent({ initialState: { model, tools: [], systemPrompt: ["Reply OK."] }, cwd,
  getApiKey: () => "fixture-token", convertToLlm: convertOmpMessages,
  streamFn: (m, c, o) => { activeKind = "model"; return measuredModel(m, c, { ...o, fetch: fakeFetch }); },
});
const adapter = await createOmpSession({ agent, sessionManager: journal.manager, stateDir: root,
  tokenFor: provider => provider === model.provider ? "fixture-token" : undefined,
  sideStreamFn: (m, c, o) => { activeKind = "compaction"; return measuredSide(m, c, { ...o, fetch: fakeFetch }); },
  nativeCompactionOptions: candidate => {
    factoryModels.push(candidate.id);
    return { fetch: fakeFetch, onProviderAttempt: usage.observer("compaction", candidate, attribution()) };
  },
});
// Remote first, then the local summary. The adapter must not add a bundled
// sibling model: that call would leave the host's fixture transport.
adapter.settings.set("compaction.methodOrder", ["remote", "soft"]);
adapter.settings.set("compaction.keepRecentTokens", 1000);
try {
  if (automatic) {
    await adapter.session.promptCustomMessage({ customType: "claudia-request", content: "Continue.", display: true,
      attribution: "user", details: attribution().requests[0] });
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
  assert.ok(finals.every(fact => fact.turn_id === "turn-A" && JSON.stringify(fact.requests) === JSON.stringify(attribution().requests)));
  assert.ok(factoryModels.every(id => id === model.id));
  const routes = calls.map(call => call.route);
  const summaries = finals.filter((fact, index) => calls[index].route === "summary" && fact.kind === "compaction");
  const modelTurns = finals.filter(fact => fact.kind === "model");
  if (local) {
    assert.deepEqual(routes.slice(0, backend === "codex" ? 1 : 2), backend === "codex" ? ["v2"] : ["v2", "v1"]);
    assert.ok(!routes.includes("v1") || backend !== "codex");
    assert.deepEqual(finals.slice(0, backend === "codex" ? 1 : 2).map(fact => fact.state), backend === "codex" ? ["failed"] : ["failed", "failed"]);
    if (backend !== "codex") assert.equal(finals[1].counters.total_input_tokens, 20);
    else assert.deepEqual(finals[0].counters, { total_input_tokens: 12, output_tokens: 0 });
    assert.ok(summaries.length > 0);
    assert.ok(summaries.every(fact => fact.state === "completed" && fact.counters.total_input_tokens === 28));
    assert.equal(new Set(summaries.map(fact => fact.invocation_id)).size, summaries.length);
  } else if (scenario === "absent") {
    assert.deepEqual(routes, automatic ? ["v2", "summary"] : ["v2"]);
    assert.equal(finals[0].state, "completed");
    assert.deepEqual(finals[0].counters, {});
  } else {
    assert.deepEqual(routes, automatic ? ["v2", "summary"] : ["v2"]);
    assert.equal(finals[0].state, "completed");
    assert.deepEqual(finals[0].counters, { total_input_tokens: 100, input_tokens: 60, output_tokens: 7, cache_read_input_tokens: 40, cache_creation_input_tokens: 0, reasoning_tokens: 0 });
    assert.equal(finals[0].provider_response_id, "compact_v2");
  }
  assert.equal(modelTurns.length, automatic ? 1 : 0);
  if (automatic) assert.equal(modelTurns[0].counters.total_input_tokens, 28);
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
