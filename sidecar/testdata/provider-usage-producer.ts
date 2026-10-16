// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Real seat/engine/provider parser, with a completely in-memory fake network.
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { createSeatAgent } from "../seat.ts";
import { SeatStore } from "../store.ts";

const root = process.argv[2];
const model = { ...getBundledModel("xai-oauth", "grok-4.3")!, baseUrl: "https://fixture.invalid" };
let calls = 0;
globalThis.fetch = (async (input: string | URL | Request) => {
  const url = input instanceof Request ? input.url : String(input);
  if (!url.startsWith("https://fixture.invalid/")) throw new Error("unexpected fixture URL");
  calls++;
  if (calls > 2) throw new Error("unexpected additional fixture request");
  const path = calls === 1 ? "response-empty.sse" : "responses.sse";
  return new Response(readFileSync(join(import.meta.dir, "provider-usage", path), "utf8"), {
    headers: { "content-type": "text/event-stream", "x-request-id": `http-${calls}` },
  });
}) as typeof fetch;
const agent = createSeatAgent({ provider: model.provider, model: model.id, token: "fixture", cwd: root,
  modelOverride: model, store: new SeatStore(join(root, "omp-seats"), "seat", "generation"), emit: () => {}, callTool: async () => "" });
await agent.prompt("Fixture question", { request_id: "logical-A", attempt_id: "attempt-A", session_id: "generation", turn_id: "turn-A" });
if (calls !== 2) throw new Error(`expected two physical requests, got ${calls}`);
console.log(JSON.stringify(agent.usage()));
