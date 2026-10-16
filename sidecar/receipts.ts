// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import type { UserMessage } from "@oh-my-pi/pi-ai";
import type { History } from "./maintain.ts";

export const requestFactType = "claudia_request_v1";
export type ReceiptDelivery = "prompt" | "steer";
export type ReceiptTerminalState = "completed" | "cancelled" | "failed";
export type ReceiptState = "accepted" | "started" | ReceiptTerminalState | "unknown";
export type ReceiptMeta = { request_id: string; session_id: string; attempt_id?: string };
export type ReceiptMessage = UserMessage & { claudiaRequest: { request_id: string; attempt_id: string } };
export type RequestReceipt = {
  request_id: string;
  session_id: string;
  seat: string;
  state: ReceiptState;
  attempt_id: string;
  turn_ids: string[];
  reason?: string;
  // Unknown queries for IDs not in the journal have no acceptance time.
  accepted_at?: string;
  updated_at?: string;
};
type Fact = {
  receipt: RequestReceipt;
  text: string;
  delivery: ReceiptDelivery;
  message_timestamp: number;
};
export type PendingReceipt = { message: ReceiptMessage; delivery: ReceiptDelivery; receipt: RequestReceipt };

const states = new Set<ReceiptState>(["accepted", "started", "completed", "cancelled", "failed", "unknown"]);
const terminals = new Set<ReceiptState>(["completed", "cancelled", "failed"]);
const nonempty = (v: unknown): v is string => typeof v === "string" && v.trim().length > 0;
const timestamp = (v: unknown): v is string => typeof v === "string" && Number.isFinite(Date.parse(v));

// Facts, rather than conversation text or transient engine events, own request
// acceptance. This is a projection over History's journal, not another queue:
// pending() supplies accepted inputs to the existing Agent queues on recovery.
export class ReceiptJournal {
  #history: History;
  #facts = new Map<string, Fact>();
  #poison?: string;

  constructor(history: History) {
    this.#history = history;
    for (const entry of history.entries) {
      if (!("customType" in entry) || entry.customType !== requestFactType) continue;
      if (entry.type !== "custom") throw new Error("request journal: invalid fact entry type");
      const fact = this.#validate(entry.data);
      const previous = this.#facts.get(fact.receipt.request_id);
      this.#validateTransition(previous, fact);
      this.#facts.set(fact.receipt.request_id, structuredClone(fact));
    }
    // A started request may already have performed external effects. Keep the
    // stored fact intact and expose uncertainty without automatically replaying.
    for (const fact of this.#facts.values()) {
      if (fact.receipt.state === "started") {
        fact.receipt.state = "unknown";
        fact.receipt.reason = "interrupted";
      }
    }
  }

  accept(text: string, meta: ReceiptMeta, delivery: ReceiptDelivery): { fresh: boolean; receipt: RequestReceipt; message: ReceiptMessage } {
    this.#writable();
    if (typeof text !== "string" || !meta || !nonempty(meta.request_id) || !nonempty(meta.session_id) ||
        (meta.attempt_id !== undefined && !nonempty(meta.attempt_id)) || (delivery !== "prompt" && delivery !== "steer")) {
      throw new Error("request journal: invalid identified request");
    }
    const store = this.#history.store!;
    if (meta.session_id !== store.session) throw new Error("request journal: session identity mismatch");
    const old = this.#facts.get(meta.request_id);
    if (old) {
      if (old.text !== text) throw new Error(`request journal: conflicting payload for ${meta.request_id}`);
      // Delivery escalation and an ack retry retain the original obligation.
      return { fresh: false, receipt: structuredClone(old.receipt), message: this.#message(old) };
    }
    const now = Date.now();
    const fact: Fact = {
      text, delivery, message_timestamp: now,
      receipt: {
        request_id: meta.request_id, session_id: store.session, seat: store.seat,
        state: "accepted", attempt_id: meta.attempt_id ?? crypto.randomUUID(), turn_ids: [],
        accepted_at: new Date(now).toISOString(), updated_at: new Date(now).toISOString(),
      },
    };
    this.#persist(fact);
    return { fresh: true, receipt: structuredClone(fact.receipt), message: this.#message(fact) };
  }

  query(requestID: string): RequestReceipt {
    const fact = this.#facts.get(requestID);
    const reason = this.#poison ? "persistence_failed" : !this.#history.store ? "no_store" : !fact ? "not_found" : undefined;
    if (!reason) return structuredClone(fact!.receipt);
    return {
      ...(fact ? structuredClone(fact.receipt) : {
        request_id: requestID, session_id: this.#history.store?.session ?? "", seat: this.#history.store?.seat ?? "",
        attempt_id: "", turn_ids: [],
      }),
      state: "unknown", reason,
    };
  }

  pending(): PendingReceipt[] {
    this.assertHealthy();
    return [...this.#facts.values()].filter(f => f.receipt.state === "accepted").map(f => ({
      message: this.#message(f), delivery: f.delivery, receipt: structuredClone(f.receipt),
    }));
  }

  assertHealthy(): void {
    if (this.#poison) throw new Error(`request journal: persistence failed; dispatch refused: ${this.#poison}`);
  }

  // Called from the awaited pre-model gate, before dispatch. Repeated calls
  // for an already-started input may add a turn but never re-accept the input.
  start(ids: string[], turnID: string): void {
    this.#writable();
    if (!nonempty(turnID)) throw new Error("request journal: missing turn identity");
    const facts = this.#selected(ids);
    if (facts.some(f => f.receipt.state === "unknown")) throw new Error("request journal: uncertain request cannot be replayed");
    for (const old of facts) {
      if (terminals.has(old.receipt.state) || old.receipt.turn_ids.includes(turnID)) continue;
      const fact = structuredClone(old);
      fact.receipt.state = "started";
      fact.receipt.turn_ids.push(turnID);
      fact.receipt.updated_at = new Date(Math.max(Date.now(), Date.parse(old.receipt.updated_at!))).toISOString();
      this.#persist(fact);
    }
  }

  finish(ids: string[], state: ReceiptTerminalState, reason?: string): void {
    this.#writable();
    if (!terminals.has(state) || (reason !== undefined && typeof reason !== "string")) throw new Error("request journal: invalid settlement");
    for (const old of this.#selected(ids)) {
      // A turn ending cannot discharge an input still waiting in its queue.
      if (old.receipt.state !== "started") continue;
      const fact = structuredClone(old);
      fact.receipt.state = state;
      fact.receipt.updated_at = new Date(Math.max(Date.now(), Date.parse(old.receipt.updated_at!))).toISOString();
      if (reason !== undefined) fact.receipt.reason = reason;
      this.#persist(fact);
    }
  }

  #selected(ids: string[]): Fact[] {
    if (!Array.isArray(ids) || ids.some(id => !nonempty(id))) throw new Error("request journal: invalid request identities");
    return [...new Set(ids)].map(id => {
      const fact = this.#facts.get(id);
      if (!fact) throw new Error(`request journal: request ${id} was not accepted`);
      return fact;
    });
  }

  #message(fact: Fact): ReceiptMessage {
    return {
      role: "user", content: fact.text, timestamp: fact.message_timestamp,
      claudiaRequest: { request_id: fact.receipt.request_id, attempt_id: fact.receipt.attempt_id },
    };
  }

  #writable(): void {
    this.assertHealthy();
    const store = this.#history.store;
    if (!store || !nonempty(store.seat) || !nonempty(store.session)) throw new Error("request journal: identified requests require a durable seat/session store");
  }

  #persist(fact: Fact): void {
    try {
      this.#history.appendFact(requestFactType, structuredClone(fact));
    } catch (err) {
      this.#poison = err instanceof Error ? err.message : String(err);
      throw new Error(`request journal: persistence failed; dispatch refused: ${this.#poison}`);
    }
    this.#facts.set(fact.receipt.request_id, structuredClone(fact));
  }

  #validate(data: unknown): Fact {
    const f = data as Partial<Fact> | null;
    const r = f?.receipt;
    if (!f || typeof f !== "object" || typeof f.text !== "string" || (f.delivery !== "prompt" && f.delivery !== "steer") ||
        !Number.isSafeInteger(f.message_timestamp) || f.message_timestamp! < 0 || !r || typeof r !== "object" ||
        !nonempty(r.request_id) || !nonempty(r.session_id) || !nonempty(r.seat) || !nonempty(r.attempt_id) ||
        !states.has(r.state) || !Array.isArray(r.turn_ids) || r.turn_ids.some(id => !nonempty(id)) ||
        new Set(r.turn_ids).size !== r.turn_ids.length || !timestamp(r.accepted_at) || !timestamp(r.updated_at) ||
        Date.parse(r.updated_at) < Date.parse(r.accepted_at) ||
        (r.reason !== undefined && typeof r.reason !== "string") ||
        (r.state === "accepted" ? r.turn_ids.length !== 0 : r.turn_ids.length === 0)) {
      throw new Error("request journal: corrupt request fact");
    }
    const store = this.#history.store;
    if (!store || r.seat !== store.seat || r.session_id !== store.session) throw new Error("request journal: foreign seat/session fact");
    return f as Fact;
  }

  #validateTransition(old: Fact | undefined, next: Fact): void {
    const n = next.receipt;
    if (!old) {
      if (n.state !== "accepted") throw new Error("request journal: missing acceptance fact");
      return;
    }
    const p = old.receipt;
    if (old.text !== next.text || old.delivery !== next.delivery || old.message_timestamp !== next.message_timestamp ||
        p.attempt_id !== n.attempt_id || p.accepted_at !== n.accepted_at || Date.parse(n.updated_at!) < Date.parse(p.updated_at!) ||
        !p.turn_ids.every((id, index) => n.turn_ids[index] === id) ||
        (p.state === "accepted" ? n.state !== "started" : p.state !== "started" || (n.state !== "started" && !terminals.has(n.state)))) {
      throw new Error("request journal: corrupt request transition");
    }
  }
}
