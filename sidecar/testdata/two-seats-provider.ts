// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Test-only entrypoint for omp-two-seats.test.ts. Replace one catalog row with
// the registered mock API, then import the unchanged production server. No
// provider transport is called. Every dispatch is logged with the receipts
// the seat's durable store held at that instant, read from disk, so the test
// can tell a receipt closed before a dispatch that still carries its request.
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { createMockModel, MockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context } from "@oh-my-pi/pi-ai";

globalThis.fetch = (async () => { throw new Error("unexpected fixture fetch"); }) as typeof fetch;
registerMockApi();

const [sock, logPath] = [process.argv[2], process.argv[3]];
const storeRoot = join(sock, "..", "omp-seats");
const safe = (s: string) => s.replace(/[^A-Za-z0-9._-]/g, "_");
// The seat's durable receipt facts, as written on disk at this instant.
function diskReceipts(seat: string, session: string): Record<string, string> {
  const path = join(storeRoot, safe(seat), `${safe(session)}.jsonl`);
  const out: Record<string, string> = {};
  if (!existsSync(path)) return out;
  for (const line of readFileSync(path, "utf8").split("\n")) {
    if (!line.trim()) continue;
    const entry = JSON.parse(line);
    if (entry.type === "custom" && entry.customType === "claudia_request_v1") out[entry.data.receipt.request_id] = entry.data.receipt.state;
  }
  return out;
}

const textOf = (content: unknown): string => typeof content === "string" ? content
  : Array.isArray(content) ? content.map(c => (c && typeof c === "object" && "text" in c ? String((c as { text: unknown }).text) : "")).join("") : "";
const contextText = (context: Context) => JSON.stringify(context.messages);
const systemText = (context: Context) => JSON.stringify(context.systemPrompt ?? "");

// Overflow is refused once per seat, on the first dispatch that carries the
// OVERFLOW marker and no compaction summary.
const overflowed = new Set<string>();
const model = createMockModel({ provider: "anthropic", id: "claude-sonnet-5", contextWindow: 200_000, handler: (context: Context): MockResponse => {
  const all = contextText(context);
  const seat = /\[seat:([A-Z])\]/.exec(all)?.[1] ?? "?";
  const session = `s-${seat}`;
  const requests = [...new Set([...all.matchAll(/"request_id":"([^"]+)"/g)].map(m => m[1]))];
  const summarizing = /summar/i.test(systemText(context)) || /summar/i.test(textOf(context.messages.at(-1)?.content));
  const compacted = /compactionSummary|SUMMARY-OF-/.test(all);
  const last = textOf([...context.messages].reverse().find(m => m.role === "user")?.content);
  let kind = summarizing ? "summary" : "model";
  let response: MockResponse = { content: [`answer ${seat}`], usage: { input: 1000, output: 7 } };
  if (summarizing) {
    response = { content: [`SUMMARY-OF-${seat}`], usage: { input: 500, output: 3 } };
  } else if (all.includes("OVERFLOW") && !compacted && !overflowed.has(seat)) {
    overflowed.add(seat);
    kind = "overflow";
    response = { content: [], stopReason: "error", errorMessage: "prompt is too long: 250000 tokens > 200000 maximum", usage: { input: 0, output: 0 } };
  } else if (last.includes("SLOW")) {
    response = { ...response, delayMs: 1500 };
  }
  appendFileSync(logPath, JSON.stringify({
    seat, kind, requests, compacted,
    receipts: diskReceipts(seat, session),
    system: createHash("sha256").update(systemText(context)).digest("hex").slice(0, 12),
    system_has_claudia: systemText(context).includes("hosted by Claudia"),
    at: Date.now(),
  }) + "\n");
  return response;
} });
const catalog = getBundledModel("anthropic", model.id);
if (!catalog) throw new Error("fixture requires the pinned catalog row");
Object.assign(catalog, model);
Object.setPrototypeOf(catalog, MockModel.prototype);
Object.defineProperty(catalog, "calls", { value: model.calls, enumerable: false });
if (getBundledModel("anthropic", model.id)!.api !== "mock") throw new Error("catalog row was not replaced");
await import("../server.ts");
