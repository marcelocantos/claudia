// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Real seat/engine/provider parser, with a completely in-memory fake network.
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import http2 from "node:http2";
import { EventEmitter } from "node:events";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { createSeatAgent } from "../seat.ts";
import { SeatStore } from "../store.ts";

const root = process.argv[2];
const cursor = process.argv[3] === "cursor";
const codex = process.argv[3] === "codex";
const model = { ...getBundledModel(cursor ? "cursor" : codex ? "openai-codex" : "xai-oauth", cursor ? "claude-4-sonnet" : codex ? "gpt-5.5" : "grok-4.3")!, baseUrl: "https://fixture.invalid", preferWebsockets: false };
let calls = 0;
globalThis.fetch = (async (input: string | URL | Request) => {
  const url = input instanceof Request ? input.url : String(input);
  if (!url.startsWith("https://fixture.invalid/")) throw new Error("unexpected fixture URL");
  calls++;
  if (calls > 2) throw new Error("unexpected additional fixture request");
  if (codex && calls === 1) return new Response('{"error":{"message":"fixture overload"}}', { status: 429, headers: { "retry-after": "0" } });
  const path = calls === 1 ? "response-empty.sse" : "responses.sse";
  return new Response(readFileSync(join(import.meta.dir, "provider-usage", path), "utf8"), {
    headers: { "content-type": "text/event-stream", "x-request-id": `http-${calls}` },
  });
}) as typeof fetch;
if (cursor) {
 const frames = JSON.parse(readFileSync(join(import.meta.dir, "provider-usage/cursor-frames.json"), "utf8"));
 http2.connect = ((url: unknown) => {
  if (String(url) !== "https://fixture.invalid") throw new Error("unexpected H2 URL");
  const session = new EventEmitter() as any; session.close = () => {};
  session.request = () => {
   const request = new EventEmitter() as any; request.close = () => {};
   request.write = () => {
    if (++calls !== 1) throw new Error("unexpected additional Cursor request");
    queueMicrotask(() => {
     for (const name of ["context23", "delta3", "delta4", "turnEnded"]) {
      const payload = Buffer.from(frames[name], "hex"); const frame = Buffer.alloc(5 + payload.length);
      frame.writeUInt32BE(payload.length, 1); payload.copy(frame, 5); request.emit("data", frame);
     }
     request.emit("end");
    }); return true;
   }; return request;
  }; return session;
 }) as typeof http2.connect;
}
const agent = createSeatAgent({ provider: model.provider, model: model.id, token: "fixture", cwd: root,
  modelOverride: model, store: new SeatStore(join(root, "omp-seats"), "seat", "generation"), emit: () => {}, callTool: async () => "" });
await agent.prompt("Fixture question", { request_id: "logical-A", attempt_id: "attempt-A", session_id: "generation", turn_id: "turn-A" });
if (calls !== (cursor ? 1 : 2)) throw new Error(`unexpected physical request count, got ${calls}`);
console.log(JSON.stringify(agent.usage()));
