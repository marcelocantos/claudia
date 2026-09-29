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

// ContextOverflow is the reason a turn_end carries when the provider refused
// the turn as too long and compaction could not bring it back under the
// window. The host treats it as terminal (🎯T148).
export const ContextOverflow = "context_overflow";

// settings are the CLI's defaults (settings-schema.ts `compaction.*`): the
// threshold is the window less max(15% of it, 16k), 20k tokens of recent
// history are kept verbatim, and provider-native compaction is preferred.
export const settings: CompactionSettings = { ...DEFAULT_COMPACTION_SETTINGS };

// recoveryBand is the CLI's COMPACTION_RECOVERY_BAND: a threshold compaction
// has made progress only when what is left is at most this share of the
// threshold. Anything more re-trips the threshold on the next turn.
const recoveryBand = 0.8;

// transientRetries bounds how often one method's summary call is retried on
// a transient provider error before the pass gives up.
const transientRetries = 2;
const transientBackoffMs = 1000;

export type Reason = "threshold" | "overflow";
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

// History is the seat's conversation as engine entries, on one branch.
export class History {
  entries: SessionEntry[] = [];
  #ids = new WeakMap<object, string>();

  // sync records every message the Agent holds that has no entry yet. New
  // messages only ever arrive at the tail, so order is kept. A compaction
  // summary is not a message entry: its compaction entry stands for it.
  sync(messages: readonly AgentMessage[]): void {
    for (const m of messages) {
      if (m.role === "compactionSummary" || this.#ids.has(m)) continue;
      const entry: SessionMessageEntry = {
        type: "message",
        id: crypto.randomUUID(),
        parentId: this.#last(),
        timestamp: new Date(typeof m.timestamp === "number" ? m.timestamp : Date.now()).toISOString(),
        message: m,
      };
      this.entries.push(entry);
      this.#ids.set(m, entry.id);
    }
  }

  // forget drops the entry of a message the seat took back out of the
  // conversation (a refused turn before its retry).
  forget(message: AgentMessage): void {
    const id = this.#ids.get(message);
    if (!id) return;
    this.entries = this.entries.filter((e) => e.id !== id);
    this.#ids.delete(message);
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
  // buildSessionContext, non-transcript path).
  rebuild(): AgentMessage[] {
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
    const remote = openAiRemotePayload(c);
    const anthropic = getAnthropicCompactionPayload(c.preserveData);
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

export type MaintenanceEvent = { type: "compacting" | "compacted" | "compaction_warning"; text: string };

export type MaintenanceHost = {
  agent: Agent;
  token: () => string;
  emit: (ev: MaintenanceEvent) => void;
  // Tests only: the engine call, extra summary options (a fake completion),
  // and whether a model may take the provider-native lane.
  compactImpl?: typeof engineCompact;
  summaryOptions?: Partial<SummaryOptions>;
  nativeEligible?: (model: Model, s: CompactionSettings) => boolean;
};

// Maintenance runs the CLI's compaction triggers for one seat.
export class Maintenance {
  readonly history = new History();
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
  }

  get compacting(): boolean {
    return this.#abort !== null;
  }

  abort(): void {
    this.#abort?.abort();
  }

  window(): number {
    const catalog = this.#host.agent.state.model?.contextWindow ?? 0;
    return this.#limit > 0 && (catalog <= 0 || this.#limit < catalog) ? this.#limit : catalog;
  }

  noteProviderLimit(errorMessage: string | undefined): void {
    this.#limit = providerLimit(errorMessage) || this.#limit;
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

  // run compacts the conversation once and reports whether the result made
  // progress (the rebuilt context fits, or sits under the recovery band).
  // messages is the live conversation: mid-turn, the loop's own array, which
  // can hold tool results the Agent's state has not taken yet.
  async run(reason: Reason, phase: Phase, messages: readonly AgentMessage[] = this.#host.agent.state.messages): Promise<boolean> {
    const agent = this.#host.agent;
    const model = agent.state.model;
    const window = this.window();
    if (!model || window <= 0 || this.#abort) return false;
    this.history.sync(messages);
    const before = this.contextTokens(messages);
    const nativeEligible = this.#host.nativeEligible ?? shouldUseProviderNativeCompaction;
    const methods: Method[] = nativeEligible(model, methodSettings("remote")) ? ["remote", "soft"] : ["soft"];
    const compactImpl = this.#host.compactImpl ?? engineCompact;
    const abort = new AbortController();
    this.#abort = abort;
    try {
      for (const method of methods) {
        const s = methodSettings(method);
        const prep = prepareCompaction(this.history.entries, s, model, agent.tokenizer);
        if (!prep) {
          // Nothing before the newest turn can be folded: a single oversized
          // turn. Park until the engine finds a cut point again.
          this.#deadEnd = true;
          this.#host.emit({
            type: "compaction_warning",
            text: `${reason}: nothing to fold before the newest turn (${before} tokens of ${window}); compaction is paused until there is`,
          });
          return false;
        }
        this.#host.emit({ type: "compacting", text: `${reason} (${phase}): ${before} tokens of ${window}; method ${method}` });
        const codex = { operationId: crypto.randomUUID(), trigger: "auto", reason: "context_limit", phase, strategy: "memento" } as const;
        let result: CompactionResult | undefined;
        for (let attempt = 0; ; attempt++) {
          try {
            result = await compactImpl(prep, model, this.#host.token(), undefined, abort.signal, {
              convertToLlm: seatConvertToLlm,
              remoteSystemPrompt: agent.state.systemPrompt,
              initiatorOverride: "agent",
              tools: agent.state.tools,
              codexCompaction: codex as never,
              // This loop retries the whole call; the summarizer must not
              // retry too, or the two budgets multiply.
              oneshotRetry: false,
              ...this.#host.summaryOptions,
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
            this.#host.emit({ type: "compaction_warning", text: `${reason}: ${method} compaction failed: ${errorText(err)}` });
            return false;
          }
        }
        // A provider-native lane that failed falls through to a local summary.
        if (!result) continue;
        this.#commit(result, method, codex);
        const after = this.storedTokens();
        const progressed =
          reason === "overflow"
            ? after <= Math.max(0, window - resolveBudgetReserveTokens(window, settings))
            : after <= Math.floor(thresholdTokens(window) * recoveryBand);
        this.#host.emit({
          type: "compacted",
          text: `${reason} (${phase}): context ${before} -> ${after} tokens of ${window}; method ${method}`,
        });
        if (!progressed) {
          this.#deadEnd = true;
          this.#host.emit({
            type: "compaction_warning",
            text: `${reason}: compaction freed too little (${after} tokens of ${window} remain); automatic compaction is paused until there is more to fold`,
          });
        }
        return progressed;
      }
      return false;
    } finally {
      if (this.#abort === abort) this.#abort = null;
    }
  }

  #commit(result: CompactionResult, method: Method, codex: object): void {
    const agent = this.#host.agent;
    const entry = this.history.appendCompaction(result, method);
    agent.replaceMessages(this.history.rebuild());
    this.#compactedAt = Date.now();
    entry.tokensAfter = this.storedTokens();
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
