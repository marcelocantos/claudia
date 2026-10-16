// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { completeSimple, streamSimple, type AssistantMessage, type Model, type ProviderAttemptObservation } from "@oh-my-pi/pi-ai";
import type { FactJournal } from "./journal.ts";

export const usageFactType = "claudia_usage_v1";
export const usagePageLimit = 128;
export type UsageRequestRef = { request_id: string; attempt_id: string };
export type UsageCounters = {
  total_input_tokens?: number; input_tokens?: number; output_tokens?: number; cache_read_input_tokens?: number;
  cache_creation_input_tokens?: number; reasoning_tokens?: number;
};
export type UsageFact = {
  seq: number; call_id: string; seat: string; session_id: string;
  kind: "model" | "compaction"; state: "started" | "completed" | "failed" | "cancelled" | "unknown";
  provider: string; model: string; turn_id?: string; requests: UsageRequestRef[];
  invocation_id?: string; provider_attempt_id?: string; provider_request_id?: string; provider_response_id?: string;
  cause?: string; started_at: string; finished_at?: string;
  counters: UsageCounters; context_tokens?: number; protocol_token_delta_total?: number; api_equivalent_cost_usd?: number;
  counter_source: "adapter_normalized_positive" | "provider_reported"; cost_source?: "provider_adapter_estimate";
  // One stream/completion invocation can hide several transport attempts.
  coverage: "engine_invocation" | "provider_attempt";
};
export type UsagePage = { seat: string; session_id: string; facts: UsageFact[]; next: number; more: boolean };
type Attribution = { turn_id?: string; requests: UsageRequestRef[]; cause?: string };

// JSON persistence drops undefined keys. Keep the live fact in that same shape
// so a reopen compares equal to the page that was just recorded.
function omitUndefined<T extends object>(value: T): T {
  const out: Record<string, unknown> = {};
  for (const [key, item] of Object.entries(value)) if (item !== undefined) out[key] = item;
  return out as T;
}

const nonempty = (v: unknown): v is string => typeof v === "string" && v.trim().length > 0;
const natural = (v: unknown): v is number => typeof v === "number" && Number.isSafeInteger(v) && v >= 0;
const states = new Set(["started", "completed", "failed", "cancelled", "unknown"]);
const counterKeys = new Set(["total_input_tokens", "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "reasoning_tokens"]);

export function validateUsageFact(value: unknown, seat: string, session: string): UsageFact {
  const f = value as UsageFact;
  if (!f || typeof f !== "object" || f.seat !== seat || f.session_id !== session || !natural(f.seq) || f.seq === 0 ||
      !nonempty(f.call_id) || !nonempty(f.provider) || !nonempty(f.model) || !states.has(f.state) ||
      !["model", "compaction"].includes(f.kind) || !((f.coverage === "engine_invocation" && f.counter_source === "adapter_normalized_positive" && f.invocation_id === undefined && f.provider_attempt_id === undefined) ||
        (f.coverage === "provider_attempt" && f.counter_source === "provider_reported" && nonempty(f.invocation_id) && nonempty(f.provider_attempt_id) && f.call_id === f.provider_attempt_id)) ||
      !Number.isFinite(Date.parse(f.started_at)) || !Array.isArray(f.requests) || !f.counters || typeof f.counters !== "object") {
    throw new Error("usage journal: invalid identity or fact");
  }
  const ids = new Set<string>();
  for (const ref of f.requests) {
    if (!ref || !nonempty(ref.request_id) || !nonempty(ref.attempt_id) || ids.has(ref.request_id)) throw new Error("usage journal: invalid request references");
    ids.add(ref.request_id);
  }
  for (const [key, n] of Object.entries(f.counters)) {
    if (!counterKeys.has(key) || !natural(n)) throw new Error("usage journal: invalid counter");
  }
  if (f.protocol_token_delta_total !== undefined && !natural(f.protocol_token_delta_total)) throw new Error("usage journal: invalid protocol delta total");
  if (f.context_tokens !== undefined && !natural(f.context_tokens)) throw new Error("usage journal: invalid context size");
  if (f.api_equivalent_cost_usd !== undefined && (!Number.isFinite(f.api_equivalent_cost_usd) || f.api_equivalent_cost_usd < 0 || f.cost_source !== "provider_adapter_estimate")) throw new Error("usage journal: invalid cost estimate");
  if (f.state !== "started" && (!f.finished_at || !Number.isFinite(Date.parse(f.finished_at)) || Date.parse(f.finished_at) < Date.parse(f.started_at))) throw new Error("usage journal: invalid completion time");
  if (f.state === "started" && (Object.keys(f.counters).length || f.finished_at || f.context_tokens !== undefined || f.protocol_token_delta_total !== undefined || f.api_equivalent_cost_usd !== undefined)) throw new Error("usage journal: unsettled invocation has counters");
  return f;
}

export function validateUsageTransition(f: UsageFact, next: number, old?: UsageFact): void {
  if (f.seq !== next) throw new Error("usage journal: discontinuous sequence");
  if ((!old && f.state !== "started") || (old && (old.state !== "started" || f.state === "started" ||
      JSON.stringify({ ...old, seq: 0, state: "", counters: {} }) !== JSON.stringify({ ...f, seq: 0, state: "", counters: {}, finished_at: undefined, context_tokens: undefined, protocol_token_delta_total: undefined, api_equivalent_cost_usd: undefined, cost_source: undefined, provider_request_id: undefined, provider_response_id: undefined })))) {
    throw new Error("usage journal: conflicting invocation transition");
  }
}

// An indexed projection over the existing history, never a second store.
// Append failures poison reads and are reported without changing provider
// dispatch/retry policy. A missing final fact leaves consumption unknown.
export class UsageJournal {
  #history: FactJournal;
  #facts: UsageFact[] = [];
  #last = new Map<string, UsageFact>();
  #poison?: string;
  #emit: (fact: UsageFact) => void;
  #error: (message: string) => void;

  constructor(history: FactJournal, emit: (fact: UsageFact) => void = () => {}, error: (message: string) => void = () => {}) {
    this.#history = history; this.#emit = emit; this.#error = error;
    for (const e of history.entries) {
      if (e.customType !== usageFactType) continue;
      if (e.type !== "custom") throw new Error("usage journal: invalid fact entry type");
      const f = validateUsageFact(e.data, history.store?.seat ?? "", history.store?.session ?? "");
      this.#accept(f);
    }
  }

  #accept(f: UsageFact): void {
    validateUsageTransition(f, this.#facts.length + 1, this.#last.get(f.call_id));
    this.#facts.push(structuredClone(f)); this.#last.set(f.call_id, structuredClone(f));
  }

  #append(f: UsageFact): void {
    if (this.#poison) return;
    try {
      f = omitUndefined({ ...f, counters: omitUndefined(f.counters) });
      validateUsageFact(f, this.#history.store!.seat, this.#history.store!.session);
      this.#history.appendFact(usageFactType, f);
      this.#accept(f);
      this.#emit(structuredClone(f));
    } catch (err) {
      this.#fail(err);
    }
  }

  #fail(err: unknown): void {
    this.#poison = String(err);
    this.#error(`usage accounting unavailable: ${this.#poison}`);
  }

  page(after = 0, limit = usagePageLimit): UsagePage {
    if (this.#poison) throw new Error(`usage accounting unavailable: ${this.#poison}`);
    if (!natural(after) || after > this.#facts.length || !natural(limit) || limit < 1 || limit > usagePageLimit) throw new Error("invalid usage page cursor or limit");
    const facts = structuredClone(this.#facts.slice(after, after + limit));
    return { seat: this.#history.store!.seat, session_id: this.#history.store!.session, facts, next: after + facts.length, more: after + facts.length < this.#facts.length };
  }

  begin(kind: UsageFact["kind"], model: Model, attribution: Attribution): UsageFact {
    const f: UsageFact = {
      seq: this.#facts.length + 1, call_id: crypto.randomUUID(), seat: this.#history.store!.seat, session_id: this.#history.store!.session,
      kind, state: "started", provider: model.provider, model: model.id, ...structuredClone(attribution),
      started_at: new Date().toISOString(), counters: {}, counter_source: "adapter_normalized_positive", coverage: "engine_invocation",
    };
    this.#append(f); return f;
  }

  observer(kind: UsageFact["kind"], model: Model, attribution: Attribution): (event: ProviderAttemptObservation) => void {
    const invocation = crypto.randomUUID();
    const starts = new Map<string, UsageFact>();
    return event => {
      if (this.#poison) return;
      if (event.state === "started") {
        const f: UsageFact = { seq: this.#facts.length + 1, call_id: event.id, provider_attempt_id: event.id, invocation_id: invocation,
          seat: this.#history.store!.seat, session_id: this.#history.store!.session, kind, state: "started", provider: model.provider, model: model.id,
          ...structuredClone(attribution), started_at: event.startedAt, counters: {}, counter_source: "provider_reported", coverage: "provider_attempt" };
        starts.set(event.id, f); this.#append(f); return;
      }
      const start = starts.get(event.id);
      if (!start) { this.#fail("provider attempt finished without a start"); return; }
      starts.delete(event.id);
      this.#append({ ...start, seq: this.#facts.length + 1, state: event.state, finished_at: event.finishedAt,
        counters: structuredClone(event.counters), context_tokens: event.contextTokens, protocol_token_delta_total: event.protocolTokenDeltaTotal, provider_request_id: event.requestId, provider_response_id: event.responseId });
    };
  }

  finish(start: UsageFact, message?: AssistantMessage, aborted = false): void {
    if (this.#poison) return;
    const f: UsageFact = { ...start, seq: this.#facts.length + 1, finished_at: new Date().toISOString(),
      state: aborted || message?.stopReason === "aborted" ? "cancelled" : !message || message.stopReason === "error" ? "failed" : "completed", counters: {} };
    // The pinned adapters synthesize zero defaults. Only positive adapter
    // observations are usable here; absence remains unknown, not known zero.
    for (const [source, dest] of [["input", "input_tokens"], ["output", "output_tokens"], ["cacheRead", "cache_read_input_tokens"], ["cacheWrite", "cache_creation_input_tokens"], ["reasoningTokens", "reasoning_tokens"]] as const) {
      const n = message?.usage?.[source];
      if (n !== undefined && !natural(n)) { this.#fail(`invalid ${source} counter`); return; }
      if (natural(n) && n > 0) f.counters[dest] = n;
    }
    const context = message?.usage?.contextTokens;
    if (context !== undefined && !natural(context)) { this.#fail("invalid context size"); return; }
    if (natural(context)) f.context_tokens = context;
    const cost = message?.usage?.cost?.total;
    if (cost !== undefined && (typeof cost !== "number" || !Number.isFinite(cost) || cost < 0)) { this.#fail("invalid cost estimate"); return; }
    if (typeof cost === "number" && Number.isFinite(cost) && cost > 0) {
      f.api_equivalent_cost_usd = cost; f.cost_source = "provider_adapter_estimate";
    }
    this.#append(f);
  }
}

// Observe the original stream's result without proxying its iterator, local
// tool-work signal, cancellation or retry policy.
export function measuredStream(journal: () => UsageJournal | undefined, attribution: () => Attribution, kind: UsageFact["kind"] = "model"): typeof streamSimple {
  return ((model, context, options) => {
    const j = journal();
    if (!j) return streamSimple(model, context, options);
    if (model.api === "anthropic-messages" || model.api === "openai-responses" || model.api === "openai-codex-responses" || model.api === "cursor-agent") {
      return streamSimple(model, context, { ...options, onProviderAttempt: j.observer(kind, model, attribution()) });
    }
    const start = j.begin(kind, model, attribution());
    const finish = (message?: AssistantMessage) => {
      j.finish(start, message, options?.signal?.aborted);
    };
    // Abort alone is not a final accounting observation: a provider can
    // return measured usage afterward. An abandoned result remains started
    // and incomplete, just like a process interruption.
    try {
      const stream = streamSimple(model, context, options);
      void stream.result().then(finish, () => finish());
      return stream;
    } catch (err) {
      finish();
      throw err;
    }
  }) as typeof streamSimple;
}

export function measuredCompletion(journal: () => UsageJournal | undefined, attribution: () => Attribution, complete: typeof completeSimple = completeSimple): typeof completeSimple {
  return (async (model, context, options) => {
    const j = journal();
    if (j && (model.api === "anthropic-messages" || model.api === "openai-responses" || model.api === "openai-codex-responses" || model.api === "cursor-agent")) {
      return complete(model, context, { ...options, onProviderAttempt: j.observer("compaction", model, attribution()) });
    }
    const start = j?.begin("compaction", model, attribution());
    try {
      const result = await complete(model, context, options);
      if (start) j!.finish(start, result, options?.signal?.aborted);
      return result;
    } catch (err) {
      if (start) j!.finish(start, undefined, options?.signal?.aborted);
      throw err;
    }
  }) as typeof completeSimple;
}
