// Context maintenance for a long-lived sidecar seat (🎯T150).
//
// A port of the Oh My Pi CLI's automatic compaction (oh-my-pi v18.2.11:
// packages/coding-agent/src/session/session-maintenance.ts for the triggers
// and guards, session-context.ts for rebuilding the context), driving
// pi-agent-core's own compaction engine. pi-agent-core's Agent never compacts
// on its own; the CLI does it around the Agent, and so does this.
//
// The seat's history is kept as the engine's entry list: one entry per
// message, and a compaction entry that stands in for what it folded. The
// Agent's messages are always rebuilt from it after a compaction, so the
// summary, the kept tail and everything since are one consistent sequence.

import {
  calculateContextTokens,
  compact as engineCompact,
  compactionContextTokens,
  convertMessageToLlm,
  createCompactionSummaryMessage,
  DEFAULT_COMPACTION_SETTINGS,
  getAnthropicCompactionPayload,
  isProviderRefusalMessage,
  NativeCompactionError,
  prepareCompaction,
  resolveBudgetReserveTokens,
  resolveThresholdTokens,
  shouldCompact,
  shouldUseProviderNativeCompaction,
  type Agent,
  type AgentMessage,
  type CompactionEntry,
  type CompactionResult,
  type CompactionSettings,
  type SessionEntry,
  type SessionMessageEntry,
  type SummaryOptions,
} from "@oh-my-pi/pi-agent-core";
import type { AssistantMessage, Message, Model } from "@oh-my-pi/pi-ai";
import { classify, Flag, is as hasFlag, isContextOverflow } from "@oh-my-pi/pi-ai/error";
import { resetOpenAICodexHistoryAfterCompaction } from "@oh-my-pi/pi-ai/providers/openai-codex-responses";
import type { SeatStore } from "./store.ts";

// ContextOverflow is the reason a turn_end carries when the provider refused
// the turn as too long and compaction could not bring it back under the
// window. The host treats it as terminal (🎯T148).
export const ContextOverflow = "context_overflow";

// settings are the CLI's defaults (settings-schema.ts `compaction.*`): the
// threshold is the window less max(15% of it, 16k), 20k tokens of recent
// history are kept verbatim, and provider-native compaction is preferred.
export const settings: CompactionSettings = { ...DEFAULT_COMPACTION_SETTINGS };

// preserveRule is given to the summarizer on every compaction (🎯T152). A
// stock summary describes the work well and drops what the seat was told
// to keep: in the T150 live check it lost a code word it had been asked to
// remember.
export const preserveRule =
  "Keep, word for word, every fact, decision or instruction the conversation was explicitly asked to remember or keep, " +
  "and every standing direction from the owner or the host: who gave it, what it says, and any conditions. " +
  "Never shorten or drop these for brevity.";

// pinLine marks a line of a user message as a pinned fact: the sidecar
// carries it verbatim after every compaction, whatever the summary kept.
const pinLine = /^\s*PIN:\s*(.+?)\s*$/;

// pinnedMessage carries pins into the context after a compaction.
export function pinnedMessage(pins: readonly string[], timestamp: number): AgentMessage {
  const lines = pins.map((p) => `- ${p}`).join("\n");
  return {
    role: "user",
    content: [{ type: "text", text: `Pinned facts, kept verbatim across context compaction (🎯T152):\n${lines}` }],
    attribution: "agent",
    timestamp,
  } as AgentMessage;
}

// pinsIn collects the PIN: lines of user messages.
export function pinsIn(messages: readonly AgentMessage[]): string[] {
  const out: string[] = [];
  for (const m of messages) {
    if (m.role !== "user") continue;
    const c = (m as { content?: unknown }).content;
    const text =
      typeof c === "string"
        ? c
        : Array.isArray(c)
          ? c.map((x) => (x && typeof x === "object" && (x as { type?: string }).type === "text" ? String((x as { text?: unknown }).text ?? "") : "")).join("\n")
          : "";
    for (const line of text.split("\n")) {
      const hit = pinLine.exec(line);
      if (hit) out.push(hit[1]);
    }
  }
  return out;
}

// recoveryBand is the CLI's COMPACTION_RECOVERY_BAND: a threshold compaction
// has made progress only when what is left is at most this share of the
// threshold. Anything more re-trips the threshold on the next turn.
const recoveryBand = 0.8;

// transientRetries bounds how often one method's summary call is retried on
// a transient provider error before the pass gives up.
const transientRetries = 2;
const transientBackoffMs = 1000;

export type Reason = "threshold" | "overflow" | "switch";
export type Phase = "pre_turn" | "mid_turn" | "post_turn";
type Method = "remote" | "soft";

// methodSettings is the CLI's resolveMethodSettings for the two methods this
// port runs: "remote" lets compact() take a provider-native lane (Anthropic's
// compaction, Codex's compact endpoint); "soft" is a local summary only.
function methodSettings(method: Method): CompactionSettings {
  return { ...settings, strategy: "context-full", remoteEnabled: method === "remote" };
}

export function thresholdTokens(contextWindow: number): number {
  return resolveThresholdTokens(contextWindow, settings);
}

// seatConvertToLlm is what the seat's Agent sends: the Agent's own default
// drops provider refusals and any role it does not know, and a compaction
// summary is such a role — without this it would silently vanish from every
// request after a compaction.
export function seatConvertToLlm(messages: AgentMessage[]): Message[] {
  const out: Message[] = [];
  for (const m of messages) {
    if (m.role === "assistant" && isProviderRefusalMessage(m as AssistantMessage)) continue;
    const llm = convertMessageToLlm(m);
    if (llm) out.push(llm);
  }
  return out;
}

// isOverflow reports a turn the provider refused because the prompt was
// larger than the model's context window.
export function isOverflow(message: AgentMessage | undefined, contextWindow: number): boolean {
  if (!message || message.role !== "assistant") return false;
  if ((message as { stopReason?: string }).stopReason !== "error") return false;
  return isContextOverflow(message as never, contextWindow);
}

// providerLimit is the window a provider named in its refusal ("prompt is too
// long: 1058579 tokens > 1000000 maximum"), or 0. The catalog's window can
// overstate what a credential is allowed; the refusal is the provider's word.
export function providerLimit(errorMessage: string | undefined): number {
  const m = /(\d+)\s*tokens?\s*>\s*(\d+)\s*maximum/i.exec(errorMessage ?? "");
  return m ? Number(m[2]) : 0;
}

type SeatCompactionEntry = CompactionEntry & { method?: Method; tokensAfter?: number };

// entryPins are the pins a compaction entry carries (🎯T152).
function entryPins(c: CompactionEntry | undefined): string[] {
  const pins = (c?.details as { pins?: unknown } | undefined)?.pins;
  return Array.isArray(pins) ? pins.filter((p): p is string => typeof p === "string") : [];
}

// History is the seat's conversation as engine entries, on one branch.
// HistoryRollbackError is raised when switch rollback cannot prove the journal
// matches the pre-switch snapshot. Live history is rolled back in memory before
// throw; callers must not treat the switch as accepted.
export class HistoryRollbackError extends Error {
  readonly durableUncertain: boolean;

  constructor(message: string, options?: { cause?: unknown; durableUncertain?: boolean }) {
    super(message, options);
    this.name = "HistoryRollbackError";
    this.durableUncertain = options?.durableUncertain ?? true;
  }
}

export class History {
  entries: SessionEntry[] = [];
  #ids = new WeakMap<object, string>();
  // byKey finds a message's entry by content. The Agent's state and the
  // run loop's live array hold different copies of one assistant message,
  // so identity alone recorded it twice.
  #byKey = new Map<string, string>();
  // store keeps the entries on disk (🎯T151). entries[0, persisted) are
  // there already; dirty means the file no longer matches a prefix of
  // entries (one was taken back) and must be rewritten whole.
  store: SeatStore | undefined;
  #persisted = 0;
  #dirty = false;
  // streamingEntryId is the durable row for an assistant message still
  // growing while the provider streams (🎯T215). Content hash changes on
  // every partial persist; identity alone is not stable enough to replace.
  #streamingEntryId: string | undefined;

  constructor(store?: SeatStore) {
    this.store = store;
  }

  // restore loads the stored conversation, if this seat and session have
  // one, and reports whether it did. It throws when the store is unreadable.
  restore(): boolean {
    const loaded = this.store?.load();
    if (!loaded) return false;
    // A store written before messages were keyed by content can hold one
    // message twice; keep the first and rewrite the file clean.
    const kept: SessionEntry[] = [];
    for (const e of loaded) {
      if (e.type === "message") {
        const m = (e as SessionMessageEntry).message;
        const k = messageKey(m);
        if (this.#byKey.has(k)) continue;
        this.#byKey.set(k, e.id);
        this.#ids.set(m, e.id);
      }
      kept.push(e);
    }
    this.entries = kept;
    this.#persisted = kept.length;
    this.#dirty = kept.length !== loaded.length;
    return true;
  }

  // durableSnapshot clones the entry list for switch-time rollback (🎯T243).
  durableSnapshot(): SessionEntry[] {
    return JSON.parse(JSON.stringify(this.entries)) as SessionEntry[];
  }

  // beginSwitchDurableBackup copies the on-disk journal before switch-time fold.
  beginSwitchDurableBackup(): void {
    this.store?.beginSwitchBackup();
  }

  clearSwitchDurableBackup(): void {
    this.store?.clearSwitchBackup();
  }

  // rollbackDurable restores a snapshot. memoryOnly skips rewrite when the
  // trial fold was never persisted (switch refused before viability).
  rollbackDurable(snapshot: SessionEntry[], options: { memoryOnly?: boolean } = {}): void {
    const restored = JSON.parse(JSON.stringify(snapshot)) as SessionEntry[];
    this.#replaceEntries(restored);
    if (this.store && !options.memoryOnly) {
      try {
        this.store.rewrite(restored);
      } catch (err) {
        if (this.#tryReconcileDiskWithSnapshot(restored)) return;
        throw new HistoryRollbackError(
          "conversation store rollback failed; durable history may still include the switch-time compaction",
          { cause: err, durableUncertain: true },
        );
      }
    }
  }

  #journalMatchesSnapshot(snapshot: SessionEntry[]): boolean {
    if (!this.store) return true;
    try {
      const loaded = this.store.load({ repairTail: false });
      if (!loaded) return snapshot.length === 0;
      return JSON.stringify(loaded) === JSON.stringify(snapshot);
    } catch {
      return false;
    }
  }

  // tryReconcileDiskWithSnapshot restores from pre-switch backup or verifies the
  // journal was never mutated when commit rewrite failed atomically.
  #tryReconcileDiskWithSnapshot(snapshot: SessionEntry[]): boolean {
    if (this.#journalMatchesSnapshot(snapshot)) return true;
    try {
      this.store!.restoreSwitchBackup();
    } catch {
      // backup missing or restore failed
    }
    return this.#journalMatchesSnapshot(snapshot);
  }

  #replaceEntries(restored: SessionEntry[]): void {
    this.entries = restored;
    this.#ids = new WeakMap();
    this.#byKey = new Map();
    for (const e of this.entries) {
      if (e.type === "message") {
        const m = (e as SessionMessageEntry).message;
        const k = messageKey(m);
        this.#byKey.set(k, e.id);
        this.#ids.set(m, e.id);
      }
    }
    this.#streamingEntryId = undefined;
    this.#persisted = this.entries.length;
    this.#dirty = false;
  }

  // persist writes what the store does not have yet: the new tail, or the
  // whole list when an entry was taken back.
  persist(messages?: readonly AgentMessage[]): void {
    if (messages) this.sync(messages);
    if (!this.store) return;
    if (this.#dirty) {
      this.store.rewrite(this.entries);
      this.#dirty = false;
    } else {
      this.store.append(this.entries.slice(this.#persisted));
    }
    this.#persisted = this.entries.length;
  }

  // persistSwitchCommit replaces the whole journal atomically (tmp+rename).
  // Switch viability must not use append for the trial fold commit: append can
  // leave a partial or complete compaction line while the switch is still refused.
  persistSwitchCommit(messages?: readonly AgentMessage[]): void {
    if (messages) this.sync(messages);
    if (!this.store) return;
    this.store.rewrite(this.entries);
    this.#persisted = this.entries.length;
    this.#dirty = false;
  }

  // Execution facts share the conversation's durable entry sequence but
  // never enter model context. Persist before acknowledging the fact.
  appendFact(customType: string, data: unknown): void {
    if (!this.store) throw new Error("durable execution facts require a session store");
    this.entries.push({
      type: "custom", customType, data,
      id: crypto.randomUUID(), parentId: this.#last(),
      timestamp: new Date().toISOString(),
    });
    this.persist();
  }

  // latestCompactionAt is when the newest compaction entry was committed.
  latestCompactionAt(): number {
    for (let i = this.entries.length - 1; i >= 0; i--) {
      if (this.entries[i].type === "compaction") return new Date(this.entries[i].timestamp).getTime();
    }
    return 0;
  }

  // sync records every message the Agent holds that has no entry yet. New
  // messages only ever arrive at the tail, so order is kept. A compaction
  // summary is not a message entry: its compaction entry stands for it.
  sync(messages: readonly AgentMessage[]): void {
    for (const m of messages) {
      if (m.role === "compactionSummary" || this.#ids.has(m)) continue;
      const k = messageKey(m);
      const known = this.#byKey.get(k);
      if (known) {
        this.#ids.set(m, known);
        continue;
      }
      const entry: SessionMessageEntry = {
        type: "message",
        id: crypto.randomUUID(),
        parentId: this.#last(),
        timestamp: new Date(typeof m.timestamp === "number" ? m.timestamp : Date.now()).toISOString(),
        message: m,
      };
      this.entries.push(entry);
      this.#ids.set(m, entry.id);
      this.#byKey.set(k, entry.id);
    }
  }

  // forget drops the entry of a message the seat took back out of the
  // conversation (a refused turn before its retry).
  forget(message: AgentMessage): void {
    const id = this.#ids.get(message);
    if (!id) return;
    if (this.#streamingEntryId === id) this.#streamingEntryId = undefined;
    const at = this.entries.findIndex((e) => e.id === id);
    if (at < this.#persisted) this.#dirty = true;
    this.entries = this.entries.filter((e) => e.id !== id);
    this.#persisted = Math.min(this.#persisted, this.entries.length);
    this.#ids.delete(message);
    this.#byKey.delete(messageKey(message));
  }

  // clearInFlightAssistant ends the open streaming/synthetic assistant slot
  // without removing its durable entry (the turn finished or moved on).
  clearInFlightAssistant(): void {
    this.#streamingEntryId = undefined;
  }

  // upsertInFlightAssistant records one assistant message that may grow across
  // mid-stream persists (🎯T215). Updates the same store row instead of
  // appending a parent-linked chain of partial copies.
  upsertInFlightAssistant(m: AgentMessage): void {
    let existingId = this.#streamingEntryId ?? this.#ids.get(m);
    if (existingId) {
      const at = this.entries.findIndex((e) => e.id === existingId);
      if (at >= 0 && this.entries[at].type === "message") {
        const entry = this.entries[at] as SessionMessageEntry;
        const oldMsg = entry.message;
        if (oldMsg !== m) {
          this.#byKey.delete(messageKey(oldMsg));
          this.#ids.delete(oldMsg);
        }
        entry.message = m;
        entry.timestamp = new Date(typeof m.timestamp === "number" ? m.timestamp : Date.now()).toISOString();
        if (at < this.#persisted) this.#dirty = true;
        this.#ids.set(m, existingId);
        this.#byKey.set(messageKey(m), existingId);
        this.#streamingEntryId = existingId;
        return;
      }
    }
    const entry: SessionMessageEntry = {
      type: "message",
      id: crypto.randomUUID(),
      parentId: this.#last(),
      timestamp: new Date(typeof m.timestamp === "number" ? m.timestamp : Date.now()).toISOString(),
      message: m,
    };
    this.entries.push(entry);
    this.#ids.set(m, entry.id);
    this.#byKey.set(messageKey(m), entry.id);
    this.#streamingEntryId = entry.id;
  }

  // latestCompaction is the newest compaction entry, if any.
  latestCompaction(): CompactionEntry | undefined {
    for (let i = this.entries.length - 1; i >= 0; i--) {
      if (this.entries[i].type === "compaction") return this.entries[i] as CompactionEntry;
    }
    return undefined;
  }

  appendCompaction(result: CompactionResult, method: Method): SeatCompactionEntry {
    const entry: SeatCompactionEntry = {
      type: "compaction",
      id: crypto.randomUUID(),
      parentId: this.#last(),
      timestamp: new Date().toISOString(),
      summary: result.summary,
      shortSummary: result.shortSummary,
      firstKeptEntryId: result.firstKeptEntryId,
      tokensBefore: result.tokensBefore,
      details: result.details,
      preserveData: result.preserveData,
      method,
    };
    this.entries.push(entry);
    return entry;
  }

  // rebuild is the model's context from the entries: the latest compaction's
  // summary, the tail it kept, and everything after it (the CLI's
  // buildSessionContext, non-transcript path). provider is the provider the
  // context is for: an OpenAI replacement history from another provider is
  // opaque to it, so the summary text and the kept turns stand in for it
  // (🎯T195, a seat that switched provider).
  rebuild(provider?: string): AgentMessage[] {
    const path = this.entries;
    let ci = -1;
    for (let i = path.length - 1; i >= 0; i--) {
      if (path[i].type === "compaction") {
        ci = i;
        break;
      }
    }
    const messageOf = (e: SessionEntry) => (e.type === "message" ? [(e as SessionMessageEntry).message] : []);
    if (ci < 0) return path.flatMap(messageOf);
    const c = path[ci] as SeatCompactionEntry;
    const native = openAiRemotePayload(c);
    const remote = native && (!provider || native.provider === provider) ? native : undefined;
    const anthropic = !provider || provider === "anthropic" ? getAnthropicCompactionPayload(c.preserveData) : undefined;
    // A natively replayed Anthropic summary must predate the kept tail, or
    // its rewrite marker would strip the tail's thinking signatures.
    let summaryTimestamp = c.timestamp;
    const firstKept = path.findIndex((e, i) => i < ci && e.id === c.firstKeptEntryId);
    if (anthropic !== undefined) {
      const retained = (firstKept >= 0 ? path[firstKept] : undefined) ?? path[ci + 1];
      const at = retained ? new Date(retained.timestamp).getTime() : Number.NaN;
      if (Number.isFinite(at)) summaryTimestamp = new Date(at - 1).toISOString();
    }
    const out: AgentMessage[] = [
      createCompactionSummaryMessage(c.summary, c.tokensBefore, summaryTimestamp, {
        shortSummary: c.shortSummary,
        providerPayload: remote ?? anthropic,
        method: c.method,
        tokensAfter: c.tokensAfter,
      }),
    ];
    // Pinned facts follow the summary verbatim, whatever it kept (🎯T152).
    // A message of their own: a native payload can stand in for the summary
    // text, never for these.
    const pins = entryPins(c);
    if (pins.length > 0) out.push(pinnedMessage(pins, new Date(summaryTimestamp).getTime()));
    // An OpenAI replacement history already carries the kept turns.
    if (!remote && firstKept >= 0) {
      for (let i = firstKept; i < ci; i++) out.push(...messageOf(path[i]));
    }
    for (let i = ci + 1; i < path.length; i++) out.push(...messageOf(path[i]));
    return out;
  }

  #last(): string | null {
    return this.entries.length > 0 ? this.entries[this.entries.length - 1].id : null;
  }
}

// messageKey identifies a message by content: its role, time and a hash of
// the message. Two copies of one message share it. The coding-agent session
// that v0.54-v0.58 adopted beside the seat stamped completedAt and
// contextSnapshot on its own assistant copy after the turn-end hook had
// journaled the engine's copy. Those fields stay out of the hash so a store
// written then still resumes each message once.
function messageKey(m: AgentMessage): string {
  const stamp = m as AgentMessage & { completedAt?: unknown; contextSnapshot?: unknown; attribution?: unknown };
  const { completedAt: _completedAt, contextSnapshot: _contextSnapshot, attribution: _attribution, ...body } = stamp;
  return `${m.role}|${typeof m.timestamp === "number" ? m.timestamp : ""}|${Bun.hash(JSON.stringify(body))}`;
}

// openAiRemotePayload reads a validated OpenAI Responses replacement history
// from a compaction entry (the CLI's getOpenAiRemoteCompactionPayload).
function openAiRemotePayload(c: CompactionEntry) {
  const candidate = c.preserveData?.openaiRemoteCompaction as
    | { provider?: unknown; replacementHistory?: unknown }
    | undefined;
  if (!candidate || typeof candidate !== "object") return undefined;
  if (typeof candidate.provider !== "string" || candidate.provider.length === 0) return undefined;
  const items = candidate.replacementHistory;
  if (!Array.isArray(items) || !items.every((x) => x !== null && typeof x === "object")) return undefined;
  return { type: "openaiResponsesHistory" as const, provider: candidate.provider, items };
}

export type MaintenanceEvent = {
  type: "compacting" | "compacted" | "compaction_warning";
  text: string;
  context_before?: number;
  context_after?: number;
  target_window?: number;
  compact_on?: string;
  compact_provider?: string;
  compact_model?: string;
  phase?: Phase;
  compact_reason?: Reason;
};

export type SwitchCompactOn = "source" | "destination" | "explicit";

export type SwitchCompactRequest = {
  on: SwitchCompactOn;
  explicitModel?: Model;
};

export type MaintenanceHost = {
  agent: Agent;
  // store keeps the conversation durable across a sidecar restart (🎯T151).
  store?: SeatStore;
  token: () => string;
  emit: (ev: MaintenanceEvent) => void;
  // Tests only: the engine call, extra summary options (a fake completion),
  // and whether a model may take the provider-native lane.
  compactImpl?: typeof engineCompact;
  summaryOptions?: Partial<SummaryOptions>;
  // A fresh native invocation observer, using the model being compacted.
  nativeUsageObserver?: (model: Model) => SummaryOptions["onProviderAttempt"];
  nativeEligible?: (model: Model, s: CompactionSettings) => boolean;
  // preserve and pins are the host's (🎯T152): what the summary must keep
  // beyond preserveRule, and facts kept verbatim.
  preserve?: () => string | undefined;
  pins?: () => readonly string[] | undefined;
  // planToken reads a plan's access token for switch-time compaction on
  // another provider (🎯T243). Falls back to token() for the seat's own plan.
  planToken?: (provider: string) => string | undefined;
};

// Maintenance runs the CLI's compaction triggers for one seat.
export type BoundCompactResult = {
  tokensBefore: number;
  tokensAfter: number;
  committed: boolean;
};

export class Maintenance {
  readonly history: History;
  #host: MaintenanceHost;
  // limit is the window the provider itself named in an overflow refusal,
  // when it is smaller than the catalog's.
  #limit = 0;
  // compactedAt is when the latest compaction committed. Provider usage and
  // refusals from before it describe a conversation that is no longer sent.
  #compactedAt = 0;
  // deadEnd is set when a pass could not make progress. While it holds, a
  // threshold pass runs only once the engine finds something to fold again.
  #deadEnd = false;
  #abort: AbortController | null = null;

  constructor(host: MaintenanceHost) {
    this.#host = host;
    this.history = new History(host.store);
  }

  // restore rebuilds the Agent's conversation from the store, when this seat
  // and session were stored before. It throws when the store is unreadable.
  restore(): boolean {
    if (!this.history.restore()) return false;
    this.#host.agent.replaceMessages(this.history.rebuild(this.#provider()));
    this.#compactedAt = this.history.latestCompactionAt();
    return true;
  }

  // persist records the conversation so far in the store.
  persist(messages: readonly AgentMessage[] = this.#host.agent.state.messages): void {
    this.history.persist(messages);
  }

  // persistMidStream journals a partial turn: finished messages sync normally,
  // and one in-flight assistant row is updated in place (🎯T215).
  persistMidStream(messages: readonly AgentMessage[], inFlight?: AgentMessage): void {
    if (inFlight) {
      const at = messages.indexOf(inFlight);
      if (at >= 0) {
        this.history.sync(messages.slice(0, at));
        this.history.upsertInFlightAssistant(inFlight);
        this.history.sync(messages.slice(at + 1));
      } else {
        this.history.sync(messages);
        this.history.upsertInFlightAssistant(inFlight);
      }
    } else {
      this.history.sync(messages);
      this.history.clearInFlightAssistant();
    }
    this.history.persist();
  }

  get compacting(): boolean {
    return this.#abort !== null;
  }

  abort(): void {
    this.#abort?.abort();
  }

  #provider(): string | undefined {
    return this.#host.agent.state.model?.provider;
  }

  // switchModel moves the seat onto next, keeping its conversation (🎯T195).
  // A conversation too large for next's window is folded first, by the model
  // that wrote it, down to what next can carry; if it cannot be, the switch
  // is refused and the seat stays where it was. Limits learned from the old
  // provider's refusals do not describe the new one.
  async switchModel(next: Model, compact: SwitchCompactRequest = { on: "source" }): Promise<void> {
    const agent = this.#host.agent;
    const window = next.contextWindow ?? 0;
    const on = compact.on ?? "source";
    let compactor: Model | undefined;
    if (on === "destination") {
      compactor = next;
    } else if (on === "explicit") {
      if (!compact.explicitModel) {
        throw new Error("switch refused: explicit compaction requires compact_provider and compact_model");
      }
      compactor = compact.explicitModel;
    }
    if (compactor) {
      // Fail before folding when the chosen compactor has no plan auth (🎯T243).
      this.#compactorToken(compactor);
    }
    if (window > 0 && shouldCompact(this.contextTokens(agent.state.messages), window, settings)) {
      const durable = this.history.durableSnapshot();
      this.history.beginSwitchDurableBackup();
      const srcProvider = agent.state.model?.provider;
      let switchDiskCommitAttempted = false;
      try {
        await this.run("switch", "pre_turn", agent.state.messages, {
          window,
          soft: true,
          deferPersist: true,
          compactor: compactor ? { model: compactor, token: () => this.#compactorToken(compactor!) } : undefined,
          compactMeta: {
            on,
            provider: (compactor ?? agent.state.model)?.provider,
            model: (compactor ?? agent.state.model)?.id,
            window,
          },
        });
        if (shouldCompact(this.contextTokens(agent.state.messages), window, settings)) {
          throw new Error(
            `switch refused: the conversation (${this.contextTokens(agent.state.messages)} tokens) does not fit ${next.provider}/${next.id} (${window}) even after compaction`,
          );
        }
        switchDiskCommitAttempted = true;
        this.history.persistSwitchCommit();
      } catch (err) {
        let rollbackErr: unknown;
        try {
          this.history.rollbackDurable(durable, { memoryOnly: !switchDiskCommitAttempted });
        } catch (e) {
          rollbackErr = e;
        }
        if (srcProvider) agent.replaceMessages(this.history.rebuild(srcProvider));
        this.#compactedAt = this.history.latestCompactionAt();
        if (rollbackErr) throw rollbackErr;
        throw err;
      } finally {
        try {
          this.history.clearSwitchDurableBackup();
        } catch {
          // Best-effort backup cleanup must not reject a committed switch or mask rollback (🎯T243).
        }
      }
    }
    const from = agent.state.model?.provider;
    agent.setModel(next);
    this.#limit = 0;
    this.#deadEnd = false;
    if (from !== next.provider) {
      // A native compaction payload, and a Codex server-side history, belong
      // to the provider that made them.
      agent.replaceMessages(this.history.rebuild(next.provider));
      resetOpenAICodexHistoryAfterCompaction({
        providerSessionState: agent.providerSessionState,
        sessionId: agent.sessionId,
        compaction: { operationId: crypto.randomUUID(), trigger: "auto", reason: "context_limit", phase: "pre_turn", strategy: "memento" } as never,
      });
    }
  }

  #compactorToken(model: Model): string {
    const fromPlan = this.#host.planToken?.(model.provider)?.trim();
    if (fromPlan) return fromPlan;
    if (model.provider === this.#host.agent.state.model?.provider) return this.#host.token();
    throw new Error(`switch refused: no plan token for compactor provider ${model.provider}`);
  }

  window(): number {
    const catalog = this.#host.agent.state.model?.contextWindow ?? 0;
    return this.#limit > 0 && (catalog <= 0 || this.#limit < catalog) ? this.#limit : catalog;
  }

  noteProviderLimit(errorMessage: string | undefined): void {
    this.#limit = providerLimit(errorMessage) || this.#limit;
  }

  // noteObservedLimit records a prompt size the provider would not answer
  // without saying why (a repeated hollow length stop). It only ever lowers
  // the window.
  noteObservedLimit(tokens: number): void {
    if (tokens > 0 && (!this.#limit || tokens < this.#limit)) this.#limit = tokens;
  }

  // storedTokens is the local estimate of what the next request carries:
  // system prompt, tool schemas and messages.
  storedTokens(messages: readonly AgentMessage[] = this.#host.agent.state.messages): number {
    const agent = this.#host.agent;
    const prefix = [...(agent.state.systemPrompt ?? [])];
    for (const t of agent.state.tools ?? []) {
      prefix.push(t.name, t.description ?? "", JSON.stringify(t.parameters ?? {}));
    }
    return agent.tokenizer.countTokens(prefix) + agent.tokenizer.countMessages(messages, { excludeEncryptedReasoning: true });
  }

  // contextTokens is the CLI's compaction measure: the larger of the
  // provider's last report and the local estimate, plus a pending prompt.
  contextTokens(messages: readonly AgentMessage[], pending = 0): number {
    let provider = 0;
    for (let i = messages.length - 1; i >= 0; i--) {
      const m = messages[i] as AssistantMessage;
      if (m.role !== "assistant" || !m.usage) continue;
      if ((m.timestamp ?? 0) >= this.#compactedAt) provider = calculateContextTokens(m.usage);
      break;
    }
    return compactionContextTokens(provider, this.storedTokens(messages)) + pending;
  }

  // due reports whether a threshold pass should run now.
  due(messages: readonly AgentMessage[], pending = 0): boolean {
    const window = this.window();
    if (window <= 0 || !shouldCompact(this.contextTokens(messages, pending), window, settings)) return false;
    if (!this.#deadEnd) return true;
    // Parked at a dead end: re-arm only once there is something to fold.
    const model = this.#host.agent.state.model;
    this.history.sync(messages);
    if (!model || !prepareCompaction(this.history.entries, methodSettings("soft"), model, this.#host.agent.tokenizer)) {
      return false;
    }
    this.#deadEnd = false;
    return true;
  }

  // compactToBound is the synchronous one-shot host compaction API (🎯T244).
  async compactToBound(boundTokens: number): Promise<BoundCompactResult> {
    if (!Number.isFinite(boundTokens) || boundTokens <= 0) {
      throw Object.assign(new Error("compact refused: bound_tokens must be positive"), { reason: "invalid_bound" });
    }
    const messages = this.#host.agent.state.messages;
    const before = this.contextTokens(messages);
    if (before <= boundTokens) {
      return { tokensBefore: before, tokensAfter: before, committed: false };
    }
    if (this.#abort) {
      throw Object.assign(new Error("compact refused: compaction already running"), { reason: "turn_in_flight" });
    }
    const progressed = await this.run("bound", "pre_turn", messages, { window: boundTokens, compactOn: "bound" });
    const after = this.contextTokens(this.#host.agent.state.messages);
    const committed = progressed || after < before;
    if (!committed && after > boundTokens) {
      throw Object.assign(
        new Error("compact refused: no eligible fold or insufficient progress"),
        { reason: "no_eligible_fold" },
      );
    }
    return { tokensBefore: before, tokensAfter: after, committed };
  }

  // run compacts the conversation once and reports whether the result made
  // progress (the rebuilt context fits, or sits under the recovery band).
  // messages is the live conversation: mid-turn, the loop's own array, which
  // can hold tool results the Agent's state has not taken yet.
  // target narrows a pass: the window to fold down to (a switch to a smaller
  // model) and whether only the local summary may run, since a native
  // payload would be foreign to the model taking over.
  async run(
    reason: Reason,
    phase: Phase,
    messages: readonly AgentMessage[] = this.#host.agent.state.messages,
    target: {
      window?: number;
      soft?: boolean;
      deferPersist?: boolean;
      compactor?: { model: Model; token: () => string };
      compactMeta?: { on: SwitchCompactOn; provider?: string; model?: string; window: number };
    } = {},
  ): Promise<boolean> {
    const agent = this.#host.agent;
    const seatModel = agent.state.model;
    const model = target.compactor?.model ?? seatModel;
    const window = target.window ?? this.window();
    if (!model || window <= 0 || this.#abort) return false;
    this.history.sync(messages);
    const before = this.contextTokens(messages);
    const meta = target.compactMeta ?? { on: "source" as const, provider: seatModel?.provider, model: seatModel?.id, window };
    const progress = (type: MaintenanceEvent["type"], text: string, after?: number) =>
      this.#host.emit({
        type,
        text,
        context_before: before,
        context_after: after,
        target_window: window,
        compact_on: meta.on,
        compact_provider: meta.provider,
        compact_model: meta.model,
        phase,
        compact_reason: reason,
      });
    const nativeEligible = this.#host.nativeEligible ?? shouldUseProviderNativeCompaction;
    const methods: Method[] = !target.soft && nativeEligible(model, methodSettings("remote")) ? ["remote", "soft"] : ["soft"];
    const compactImpl = this.#host.compactImpl ?? engineCompact;
    const token = target.compactor?.token ?? this.#host.token;
    const abort = new AbortController();
    this.#abort = abort;
    try {
      for (const method of methods) {
        const s = methodSettings(method);
        const prep = prepareCompaction(this.history.entries, s, model, agent.tokenizer);
        if (!prep) {
          this.#deadEnd = true;
          progress("compaction_warning", `${reason}: nothing to fold before the newest turn (${before} tokens of ${window}); compaction is paused until there is`);
          return false;
        }
        progress("compacting", `${reason} (${phase}): ${before} tokens of ${window}; method ${method}`);
        const codex = { operationId: crypto.randomUUID(), trigger: "auto", reason: "context_limit", phase, strategy: "memento" } as const;
        const hostPreserve = this.#host.preserve?.()?.trim();
        const instructions = hostPreserve ? `${preserveRule}\n${hostPreserve}` : preserveRule;
        let result: CompactionResult | undefined;
        for (let attempt = 0; ; attempt++) {
          try {
            result = await compactImpl(prep, model, token(), instructions, abort.signal, {
              convertToLlm: seatConvertToLlm,
              remoteSystemPrompt: agent.state.systemPrompt,
              initiatorOverride: "agent",
              tools: agent.state.tools,
              codexCompaction: codex as never,
              // This loop retries the whole call; the summarizer must not
              // retry too, or the two budgets multiply.
              oneshotRetry: false,
              ...this.#host.summaryOptions,
              onProviderAttempt: this.#host.nativeUsageObserver?.(model),
            });
            break;
          } catch (err) {
            if (abort.signal.aborted) throw err;
            if (err instanceof NativeCompactionError) break;
            const id = classify(err, model.api);
            if (attempt < transientRetries && hasFlag(id, Flag.Transient)) {
              await Bun.sleep(transientBackoffMs * (attempt + 1));
              continue;
            }
            progress("compaction_warning", `${reason}: ${method} compaction failed: ${errorText(err)}`);
            return false;
          }
        }
        // A provider-native lane that failed falls through to a local summary.
        if (!result) continue;
        // Pins: those already carried, those the conversation marked, and
        // the host's, in that order, each once (🎯T152).
        const pins = [
          ...new Set([
            ...entryPins(this.history.latestCompaction()),
            ...pinsIn(messages),
            ...(this.#host.pins?.() ?? []),
          ]),
        ];
        if (pins.length > 0) result = { ...result, details: { ...(result.details as object | undefined), pins } };
        this.#commit(result, method, codex, { persist: !target.deferPersist });
        const after = this.storedTokens();
        const progressed =
          reason === "overflow"
            ? after <= Math.max(0, window - resolveBudgetReserveTokens(window, settings))
            : after <= Math.floor(thresholdTokens(window) * recoveryBand);
        progress("compacted", `${reason} (${phase}): context ${before} -> ${after} tokens of ${window}; method ${method}`, after);
        if (!progressed) {
          this.#deadEnd = true;
          progress("compaction_warning", `${reason}: compaction freed too little (${after} tokens of ${window} remain); automatic compaction is paused until there is more to fold`, after);
        }
        return progressed;
      }
      return false;
    } finally {
      if (this.#abort === abort) this.#abort = null;
    }
  }

  #commit(result: CompactionResult, method: Method, codex: object, options: { persist?: boolean } = {}): void {
    const agent = this.#host.agent;
    const entry = this.history.appendCompaction(result, method);
    agent.replaceMessages(this.history.rebuild(this.#provider()));
    this.#compactedAt = Date.now();
    entry.tokensAfter = this.storedTokens();
    if (options.persist !== false) this.history.persist();
    // A Codex seat's server-side history no longer matches what is sent.
    resetOpenAICodexHistoryAfterCompaction({
      providerSessionState: agent.providerSessionState,
      sessionId: agent.sessionId,
      compaction: codex as never,
    });
  }
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
