// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { History } from "./maintain.ts";
import { SeatStore } from "./store.ts";
import { measuredStream, UsageJournal } from "./usage.ts";

const originalSocket = globalThis.WebSocket;
const roots: string[] = [];
const sockets: FakeSocket[] = [];
afterEach(() => {
  for (const socket of sockets.splice(0)) socket.close();
  FakeSocket.failHandshake = false;
  globalThis.WebSocket = originalSocket;
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});
const specimen = (name: string) => readFileSync(join(import.meta.dir, "testdata/provider-usage", name), "utf8");
const frames = (name = "responses.sse") => specimen(name).split("\n").filter(line => line.startsWith("data: ")).map(line => JSON.parse(line.slice(6)));
const context = { messages: [{ role: "user" as const, content: "Fixture question", timestamp: 1 }] };
const refs = () => ({ turn_id: "turn-A", requests: [{ request_id: "logical-A", attempt_id: "delivery-A" }] });
function fixture() {
  const root = mkdtempSync(join(tmpdir(), "cl-codex-usage-")); roots.push(root);
  const store = new SeatStore(root, "seat", "generation"); const history = new History(store); history.restore();
  const errors: string[] = [];
  const journal = new UsageJournal(history, () => {}, text => errors.push(text));
  const model = { ...getBundledModel("openai-codex", "gpt-5.5")!, baseUrl: "https://fixture.invalid", preferWebsockets: true };
  return { model, journal, errors, options: { apiKey: "fixture", sessionId: crypto.randomUUID(), providerSessionState: new Map(), preferWebsockets: false, streamFirstEventTimeoutMs: 1000, streamIdleTimeoutMs: 1000 } };
}
function wire(name = "responses.sse") {
  return new Response(specimen(name), { headers: { "content-type": "text/event-stream", "x-request-id": "http-codex" } });
}
class FakeSocket {
  static OPEN = 1; static CONNECTING = 0;
  static failHandshake = false;
  readyState = 0; binaryType = "";
  onopen?: (e: any) => void; onmessage?: (e: any) => void; onerror?: (e: any) => void; onclose?: (e: any) => void;
  static onSend: (socket: FakeSocket, request: any) => void;
  constructor(url: string) {
    if (!url.startsWith("wss://fixture.invalid/")) throw new Error("unexpected websocket URL");
    sockets.push(this);
    queueMicrotask(() => {
      if (FakeSocket.failHandshake) { this.onerror?.({ type: "error", message: "fixture handshake failure" }); return; }
      this.readyState = 1; this.onopen?.({ type: "open" });
    });
  }
  send(raw: string) { queueMicrotask(() => FakeSocket.onSend(this, JSON.parse(raw))); }
  frame(value: unknown) { this.onmessage?.({ data: JSON.stringify(value) }); }
  close() { if (this.readyState === 3) return; this.readyState = 3; this.onclose?.({ code: 1006 }); }
}
function socketFixture(onSend: typeof FakeSocket.onSend) {
  FakeSocket.onSend = onSend; globalThis.WebSocket = FakeSocket as unknown as typeof WebSocket;
}

test("Codex SSE preserves raw measured zeros and physical identity", async () => {
  const f = fixture(); let calls = 0;
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, fetch: async () => { calls++; return wire(); } }).result();
  expect(result.stopReason).toBe("stop"); expect(calls).toBe(1); expect(f.errors).toEqual([]);
  const facts = f.journal.page().facts;
  expect(facts).toHaveLength(2);
  expect(facts[1]).toMatchObject({ coverage: "provider_attempt", state: "completed", provider_request_id: "http-codex", counters: {
    total_input_tokens: 28, input_tokens: 21, output_tokens: 4, cache_read_input_tokens: 7, cache_creation_input_tokens: 0, reasoning_tokens: 0,
  } });
  expect(facts[1].requests).toEqual(refs().requests);
});

test("Codex hidden HTTP retry retains unmeasured refusal and measured final", async () => {
  const f = fixture(); let calls = 0;
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, fetch: async () => {
    return ++calls === 1 ? new Response('{"error":{"message":"fixture overload"}}', { status: 429, headers: { "retry-after": "0" } }) : wire();
  } }).result();
  expect(result.stopReason).toBe("stop"); expect(calls).toBe(2);
  const finals = f.journal.page().facts.filter(f => f.state !== "started");
  expect(finals.map(f => f.state)).toEqual(["failed", "completed"]);
  expect(finals[0].counters).toEqual({});
  expect(new Set(finals.map(f => f.invocation_id)).size).toBe(1);
  expect(new Set(finals.map(f => f.provider_attempt_id)).size).toBe(2);
});

test("Codex failed response retains available usage before processor throws", async () => {
  const f = fixture(); let calls = 0;
  await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, fetch: async () => { calls++; return wire("response-failed.sse"); } }).result();
  expect(calls).toBe(1);
  expect(f.journal.page().facts.at(-1)).toMatchObject({ state: "failed", counters: { total_input_tokens: 12, output_tokens: 0 } });
  expect(f.journal.page().facts.at(-1)!.counters.input_tokens).toBeUndefined();
});

test("Codex actual WebSocket frame send records one physical observation", async () => {
  const f = fixture(); let sends = 0;
  socketFixture((socket, request) => { sends++; expect(request.type).toBe("response.create"); for (const frame of frames()) socket.frame(frame); });
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, preferWebsockets: true, fetch: async () => { throw new Error("unexpected HTTP fallback"); } }).result();
  expect(result.stopReason).toBe("stop"); expect(sends).toBe(1); expect(f.errors).toEqual([]);
  expect(f.journal.page().facts).toHaveLength(2);
  expect(f.journal.page().facts[1]).toMatchObject({ state: "completed", coverage: "provider_attempt", provider_response_id: "resp_fixture", counters: { total_input_tokens: 28, output_tokens: 4 } });
});

test("Codex WebSocket reconnect accounts each response.create separately", async () => {
  const f = fixture(); let sends = 0;
  socketFixture(socket => { if (++sends === 1) socket.close(); else for (const frame of frames()) socket.frame(frame); });
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, preferWebsockets: true, fetch: async () => { throw new Error("unexpected HTTP fallback"); } }).result();
  expect(result.stopReason).toBe("stop"); expect(sends).toBe(2);
  const finals = f.journal.page().facts.filter(f => f.state !== "started");
  expect(finals.map(f => f.state)).toEqual(["failed", "completed"]);
  expect(finals[0].counters).toEqual({});
  expect(new Set(finals.map(f => f.invocation_id)).size).toBe(1);
  expect(new Set(finals.map(f => f.provider_attempt_id)).size).toBe(2);
});

test("Codex WebSocket to SSE fallback retains available failed-attempt usage", async () => {
  const f = fixture(); let sends = 0; let http = 0;
  socketFixture(socket => {
    sends++;
    const partial = frames().slice(0, 3);
    partial[0].response.usage = { input_tokens: 9, output_tokens: 0 };
    for (const frame of partial) socket.frame(frame);
    socket.close();
  });
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, preferWebsockets: true, fetch: async () => { http++; return wire(); } }).result();
  expect(result.errorMessage).toBeUndefined(); expect(result.stopReason).toBe("stop"); expect(sends).toBe(1); expect(http).toBe(1);
  const finals = f.journal.page().facts.filter(f => f.state !== "started");
  expect(finals.map(f => f.state)).toEqual(["failed", "completed"]);
  expect(finals[0].counters).toEqual({ total_input_tokens: 9, output_tokens: 0 });
  expect(finals[1].counters.total_input_tokens).toBe(28);
});

test("Codex reused socket rejects stale response usage before attribution", async () => {
  const f = fixture(); let sends = 0;
  socketFixture(socket => {
    sends++;
    if (sends === 2) socket.frame({ type: "response.completed", response: { id: "resp_fixture", status: "completed", usage: { input_tokens: 99999, output_tokens: 99999 } } });
    const current = sends === 1 ? frames() : frames().map(frame => JSON.parse(JSON.stringify(frame).replaceAll("resp_fixture", "resp_second")));
    for (const frame of current) socket.frame(frame);
  });
  const options = { ...f.options, preferWebsockets: true, fetch: async () => { throw new Error("unexpected fallback"); } };
  const run = measuredStream(() => f.journal, refs);
  expect((await run(f.model, context, options).result()).stopReason).toBe("stop");
  expect((await run(f.model, { messages: [{ role: "user", content: "Second fixture", timestamp: 2 }] }, options).result()).stopReason).toBe("stop");
  expect(sockets).toHaveLength(1); expect(sends).toBe(2);
  const finals = f.journal.page().facts.filter(f => f.state !== "started");
  expect(finals.map(f => f.counters.total_input_tokens)).toEqual([28, 28]);
  expect(new Set(finals.map(f => f.invocation_id)).size).toBe(2);
});

test("Codex interleaved foreign response cannot supply accounting counters", async () => {
  const f = fixture();
  socketFixture(socket => {
    socket.frame({ type: "response.created", response: { id: "own", usage: { input_tokens: 9 } } });
    socket.frame({ type: "response.completed", response: { id: "foreign", status: "completed", usage: { input_tokens: 99999, output_tokens: 99999 } } });
  });
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, preferWebsockets: true, fetch: async () => { throw new Error("unexpected fallback"); } }).result();
  expect(result.stopReason).toBe("error");
  expect(f.journal.page().facts.at(-1)).toMatchObject({ state: "failed", counters: { total_input_tokens: 9 } });
  expect(f.journal.page().facts.at(-1)!.counters.output_tokens).toBeUndefined();
});

test("Codex compressed-request fallback records both HTTP dispatches", async () => {
  const f = fixture(); f.model = { ...getBundledModel("openai-codex", "gpt-5.5")!, preferWebsockets: true };
  const encodings: (string | null)[] = [];
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, fetch: async (_url, init) => {
    encodings.push(new Headers(init?.headers).get("content-encoding"));
    return encodings.length === 1 ? new Response("unsupported encoding", { status: 415 }) : wire();
  } }).result();
  expect(result.stopReason).toBe("stop"); expect(encodings).toEqual(["zstd", null]);
  expect(f.journal.page().facts.filter(f => f.state !== "started").map(f => f.state)).toEqual(["failed", "completed"]);
});

test("Codex failed handshake is not fabricated as a model request", async () => {
  const f = fixture(); let sends = 0; let http = 0;
  socketFixture(() => { sends++; }); FakeSocket.failHandshake = true;
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, preferWebsockets: true, fetch: async () => { http++; return wire(); } }).result();
  expect(result.stopReason).toBe("stop"); expect(sends).toBe(0); expect(http).toBe(1);
  expect(f.journal.page().facts).toHaveLength(2);
  expect(f.journal.page().facts.at(-1)!.state).toBe("completed");
});

test("Codex cancelled WebSocket retains observed partial consumption", async () => {
  const f = fixture(); const abort = new AbortController(); let sends = 0;
  socketFixture(socket => {
    sends++;
    socket.frame({ type: "response.created", response: { id: "partial", usage: { input_tokens: 9, output_tokens: 0 } } });
    queueMicrotask(() => abort.abort());
  });
  const result = await measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, signal: abort.signal, preferWebsockets: true, fetch: async () => { throw new Error("unexpected fallback"); } }).result();
  expect(result.stopReason).toBe("aborted"); expect(sends).toBe(1);
  expect(f.journal.page().facts.at(-1)).toMatchObject({ state: "cancelled", counters: { total_input_tokens: 9, output_tokens: 0 } });
});
