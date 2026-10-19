// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Actual restored seat -> automatic maintenance -> pinned native adapters.
// Every network route is in-memory and refuses any unexpected destination.
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import type { AgentMessage } from "@oh-my-pi/pi-agent-core";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { createSeatAgent, type SeatEvent } from "../seat.ts";
import { History } from "../maintain.ts";
import { SeatStore } from "../store.ts";

export async function nativeUsageFixture(root: string, scenario = "success", backend = "openai") {
  const model = { ...getBundledModel(backend === "codex" ? "openai-codex" : "openai", "gpt-5.5")!, baseUrl: "https://fixture.invalid", preferWebsockets: scenario.startsWith("websocket"), contextWindow: 100_000, remoteCompaction: { enabled: true, v2StreamingEnabled: scenario !== "v1only" } };
  if (!model.api) throw new Error("missing bundled model");
  const store = new SeatStore(join(root, "omp-seats"), "seat", "generation");
  const history = new History(store);
  // "mid" starts under the pre-prompt threshold. Its first model call is a
  // tool call that reports the occupancy crossing it, so native compaction
  // runs from the seat's turn-end hook, between that call and the next.
  const mid = scenario === "mid";
  const seedInput = mid ? 100 : 90_000;
  const messages: AgentMessage[] = [];
  for (let i = 0; i < (mid ? 4 : 7); i++) {
    messages.push({ role: "user", content: `Old turn ${i}: ${"word ".repeat(mid ? 800 : 6000)}`, timestamp: 1_000 + i * 2 });
    messages.push({ role: "assistant", content: [{ type: "text", text: "Old answer" }], api: model.api, provider: model.provider, model: model.id,
      usage: { input: seedInput, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: seedInput + 1, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop", timestamp: 1_001 + i * 2 });
  }
  history.persist(messages);
  const calls: string[] = [];
  const events: SeatEvent[] = [];
  let v2Calls = 0;
  let toolTurn = mid;
  let agent: ReturnType<typeof createSeatAgent>;
  const fakeFetch = (async (input: string | URL | Request, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    if (!url.startsWith("https://fixture.invalid/")) throw new Error(`unexpected fixture URL ${url}`);
    const body = JSON.parse(String(init?.body));
    const kind = url.endsWith("/compact") ? "v1" : body.input?.some((v: { type?: string }) => v.type === "compaction_trigger") ? "v2" : "model";
    calls.push(kind);
    if (calls.length > 8) throw new Error("unexpected extra fixture request");
    if (kind === "model" && toolTurn) {
      toolTurn = false;
      return new Response(toolCallSse, { headers: { "content-type": "text/event-stream", "x-request-id": `native-${calls.length}` } });
    }
    let file = "responses.sse";
    if (kind === "v1") file = scenario === "local" ? "compact-v1-missing.json" : "compact-v1.json";
    if (kind === "v2") {
      v2Calls++;
      file = scenario === "retry" && v2Calls === 1 ? "compact-retry.sse"
        : scenario === "v1" || scenario === "local" ? "compact-failed.sse"
        : scenario === "missing" ? "compact-missing.sse"
        : scenario === "partial" ? "compact-partial.sse" : scenario === "absent" ? "compact-absent.sse" : "compact-v2.sse";
      if (scenario === "cancel") {
        return new Response(new ReadableStream({ start(controller) {
          controller.enqueue(new TextEncoder().encode(readFileSync(join(import.meta.dir, "provider-usage/compact-progress.sse"), "utf8")));
          init?.signal?.addEventListener("abort", () => controller.error(new DOMException("Fixture cancellation", "AbortError")), { once: true });
          setTimeout(() => agent.abort(), 10);
        } }), { headers: { "content-type": "text/event-stream", "x-request-id": `native-${calls.length}` } });
      }
    }
    return new Response(readFileSync(join(import.meta.dir, "provider-usage", file), "utf8"), { headers: { "content-type": kind === "v1" ? "application/json" : "text/event-stream", "x-request-id": `native-${calls.length}` } });
  }) as typeof fetch;
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
  const originalSocket = globalThis.WebSocket;
  const sockets: FixtureSocket[] = [];
  class FixtureSocket {
    static OPEN = 1; static CONNECTING = 0;
    readyState = 0; binaryType = "";
    onopen?: (e: unknown) => void; onmessage?: (e: { data: string }) => void; onclose?: (e: { code: number }) => void;
    constructor(url: string) {
      if (!url.startsWith("wss://fixture.invalid/")) throw new Error("unexpected native WebSocket URL");
      sockets.push(this);
      queueMicrotask(() => { this.readyState = 1; this.onopen?.({ type: "open" }); });
    }
    send(raw: string) {
      const request = JSON.parse(raw);
      if (request.type !== "response.create") throw new Error("unexpected native WebSocket request");
      const native = request.input.some((v: { type?: string }) => v.type === "compaction_trigger");
      calls.push(native ? "ws" : "model");
      queueMicrotask(() => {
        const name = !native ? "responses.sse" : scenario === "websocket-fallback" ? "compact-progress.sse" : "compact-v2.sse";
        for (const line of readFileSync(join(import.meta.dir, "provider-usage", name), "utf8").split("\n")) {
          if (line.startsWith("data: ")) this.onmessage?.({ data: line.slice(6) });
        }
        if (native && scenario === "websocket-fallback") this.close();
      });
    }
    close() { if (this.readyState === 3) return; this.readyState = 3; this.onclose?.({ code: 1006 }); }
  }
  if (scenario.startsWith("websocket")) globalThis.WebSocket = FixtureSocket as unknown as typeof WebSocket;
  const originalFetch = globalThis.fetch;
  globalThis.fetch = fakeFetch;
  try {
    agent = createSeatAgent({ provider: model.provider, model: model.id, token: "fixture", cwd: root, modelOverride: model, store,
      emit: e => events.push(e), callTool: async () => "tool ok",
      tools: mid ? [{ name: "fixture_tool", description: "Continues the current provider turn.", input_schema: { type: "object", properties: {} } }] : undefined, maintenance: { summaryOptions: { fetch: fakeFetch, sessionId: "native-fixture", providerSessionState: new Map(), preferWebsockets: scenario.startsWith("websocket") } } });
    let error: unknown;
    try { await agent.prompt("Continue the old conversation", scenario === "cancel" ? undefined : { request_id: "logical-A", attempt_id: "attempt-A", session_id: "generation", turn_id: "turn-A" }); } catch (e) { error = e; }
    if (error && scenario !== "cancel") throw error;
    const page = agent.usage();
    const reopened = createSeatAgent({ provider: model.provider, model: model.id, token: "fixture", cwd: root, modelOverride: model, store, emit: () => {}, callTool: async () => "" }).usage();
    return { page, reopened, calls, events, error };
  } finally { globalThis.fetch = originalFetch; for (const socket of sockets) socket.close(); globalThis.WebSocket = originalSocket; }
}

if (import.meta.main) {
  const result = await nativeUsageFixture(process.argv[2], process.argv[3], process.argv[4]);
  console.log(JSON.stringify(result.page));
}
