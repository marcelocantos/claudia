// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Claudia supplies transport, credentials and tool policy. OMP owns the session
// loop and context maintenance; this module does not run a second turn loop.
import type { Agent, AgentMessage, StreamFn } from "@oh-my-pi/pi-agent-core";
import { AuthStorage, streamSimple, type CursorExecHandlers, type Model } from "@oh-my-pi/pi-ai";
import { copyPerCallContextMessage } from "@oh-my-pi/pi-ai/utils/block-symbols";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { AgentSession } from "@oh-my-pi/pi-coding-agent/session/agent-session";
import type { AgentSessionConfig } from "@oh-my-pi/pi-coding-agent/session/agent-session-types";
import { convertToLlm } from "@oh-my-pi/pi-coding-agent/session/messages";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { getAgentDir, getBaseConfigRoot } from "@oh-my-pi/pi-utils/dirs";
import { mkdirSync } from "node:fs";
import { isAbsolute, join, relative, sep } from "node:path";
import { cursorExecHandlers } from "./cursorexec.ts";
import type { SandboxPolicy } from "./execution.ts";
import { ReceiptJournal, type ReceiptCause, type ReceiptMessage, type ReceiptTerminalState } from "./receipts.ts";
import { toolResultBound } from "./truncate.ts";
import { measureDispatch, observedNativeCompaction, UsageJournal, type Attribution } from "./usage.ts";

// Pass this same converter to the injected Agent. Stock custom-message
// conversion discards details; host request identity must survive into the
// prepared model context without depending on message text or queue order.
// Keep OMP's complete-array conversion for all other roles and its
// neighbor-sensitive handling of interrupted thinking and compaction frames.
export function convertOmpMessages(messages: AgentMessage[]) {
  return convertToLlm(messages.map(message => {
    if (message.role !== "custom" || message.customType !== "claudia-request") return message;
    const details = message.details;
    if (!details || typeof details !== "object" ||
        !("request_id" in details) || typeof details.request_id !== "string" || !details.request_id.trim() ||
        !("attempt_id" in details) || typeof details.attempt_id !== "string" || !details.attempt_id.trim()) {
      throw new Error("OMP request message is missing its durable request/attempt identity");
    }
    const converted: ReceiptMessage = {
      role: "user",
      content: message.content,
      attribution: message.attribution ?? "user",
      timestamp: message.timestamp,
      claudiaRequest: { request_id: details.request_id, attempt_id: details.attempt_id },
    };
    copyPerCallContextMessage(converted, message);
    return converted;
  }));
}

export interface OmpSessionOptions {
  agent: Agent;
  sessionManager: SessionManager;
  // SDK globals (including blobs) must already resolve inside this private
  // root before imports. The adapter never mutates process-wide directories.
  stateDir: string;
  tokenFor: (provider: string) => string | undefined;
  // Mandatory: compaction and other SDK side calls must use the host's
  // observed transport rather than silently defaulting to streamSimple.
  sideStreamFn: StreamFn;
  // Native compaction dispatches outside sideStreamFn. Resolve its observer
  // against the actual chosen candidate; attribution remains owned by the host.
  // Mandatory: without it the SDK uses its default transport and the attempt
  // never reaches the host's usage journal.
  nativeCompactionOptions: NonNullable<AgentSessionConfig["nativeCompactionOptions"]>;
  // Seat working directory for the execution-policy Cursor handlers. The
  // coding-agent tool registry is not a substitute: built-ins stay empty.
  cwd: string;
  policy?: () => SandboxPolicy | undefined;
  // Production journal. Dispatch starts a receipt before the provider call
  // and finishes it when that agent run ends.
  receipts: ReceiptJournal;
  // Same history as receipts. The model stream records one fact per
  // invocation; side and native compaction stay on the host's own wrappers.
  usage: UsageJournal;
  // Read when the model stream starts, after the receipt start below.
  // The host supplies turn, cause and request refs.
  usageAttribution: () => Attribution;
  // The seat already owns the turn loop, Cursor handlers, usage measurement,
  // receipt bookends, credential callback, and message list. Adopt the
  // session without replacing those. The process-wide SDK directory guard
  // does not apply: this seat's store is not that directory, and the
  // session below uses in-memory settings plus this stateDir.
  preserveSeatLoop?: boolean;
  // Process-wide SDK directory root. The SDK resolves its agent and config
  // directories once per process, so several seats in one sidecar cannot
  // each own them. Defaults to stateDir (one session per process).
  // null: the caller accepts the process's own SDK directories (no guard).
  sdkRoot?: string | null;
  // Per-seat SDK setting overrides. Keys that the SDK applies process-wide
  // through its setting hooks are refused (see processGlobalSettingKeys).
  settings?: Record<string, unknown>;
  // Host Cursor exec handlers, when the host binds more than the policy
  // handlers (listed MCP tools). Defaults to the policy handlers.
  cursorExecHandlers?: CursorExecHandlers;
  // Turn identity recorded on receipt start, and a notice per receipt
  // transition, so the host can tell its consumer.
  turnId?: () => string;
  onReceipt?: (requestId: string, phase: "started" | "finished") => void;
  // Classify a terminal settle. Defaults to aborted/failed/completed from the
  // last assistant message.
  // The host's compaction body. The SDK auto-compaction path passes no
  // instructions (customInstructions: undefined) and has no setting for
  // them; its only host seam is an extension's session_before_compact
  // hook, which replaces the summary wholesale. The SDK keeps the triggers
  // (threshold, overflow, mid-turn) and the commit.
  beforeCompact?: (event: { preparation: unknown; signal: AbortSignal }) => Promise<{ compaction: unknown } | undefined>;
  // undefined: not over for the host yet; the receipts stay open.
  settle?: (messages: readonly AgentMessage[]) => { state: ReceiptTerminalState; reason?: string; cause?: ReceiptCause } | undefined;
}

// SETTING_HOOKS in pi-coding-agent/config/settings.ts that write
// process-wide state when a setting is set or overridden on ANY Settings
// instance, isolated ones included. One seat changing one of these changes
// every seat in the process. The SDK does not export the hook table, so the
// list is pinned to 18.2.11 by hand.
export const processGlobalSettingKeys: readonly string[] = [
  "providers.maxInFlightRequests", // pi-ai stream.ts configuredProviderMaxInFlightRequests
  "secrets.enabled", // pi-ai transform-messages.ts credentialRedactionEnabled
  "worktree.base", // pi-utils dirs.ts worktreesDirOverride
];
// Not refused: provider.appendOnlyContext, modelRoles, extendedContext and
// the Code Mode keys fire module-level signals that every live session
// subscribes to. Each subscriber re-reads its own Settings, so no value
// crosses seats, but one seat's change wakes every other seat's handler.

// A minimal stand-in for the SDK's ExtensionRunner: one handler,
// session_before_compact, and inert answers for the calls AgentSession
// makes on a runner (pinned to 18.2.11 by reading agent-session.ts,
// bash-runner.ts, eval-runner.ts and session-maintenance.ts).
function compactionHookRunner(hook: NonNullable<OmpSessionOptions["beforeCompact"]>) {
  return {
    hasHandlers: (type: string) => type === "session_before_compact",
    emit: async (event: { type: string; preparation?: unknown; signal?: AbortSignal }) =>
      event.type === "session_before_compact" ? hook({ preparation: event.preparation, signal: event.signal ?? new AbortController().signal }) : undefined,
    emitError: () => {},
    getCommand: () => undefined,
    createCommandContext: () => { throw new Error("Claudia seats have no extension commands"); },
    runScoped: <T>(fn: () => T) => fn(),
    emitUserBash: async () => undefined,
    emitUserPython: async () => undefined,
    emitSessionStop: async () => undefined,
    emitBeforeAgentStart: async () => undefined,
  };
}

// Refuse process-global keys on this seat's Settings instance. The SDK's own
// runtime set/override calls (steering mode, browser, skillful) are not in
// the list and still work.
function guardProcessGlobalSettings(settings: Settings): void {
  for (const method of ["set", "override", "clearOverride"] as const) {
    const original = (settings as unknown as Record<string, (...args: unknown[]) => unknown>)[method].bind(settings);
    (settings as unknown as Record<string, unknown>)[method] = (path: string, ...rest: unknown[]) => {
      if (processGlobalSettingKeys.includes(path)) {
        throw new Error(`OMP setting ${path} is process-wide in the SDK; a seat may not change it`);
      }
      return original(path, ...rest);
    };
  }
}

// The Agent stores Cursor exec handlers only at construction, and the loop
// copies that private field into the provider stream options. Overwrite the
// options on the way through so an SDK session cannot answer a Cursor frame
// with "Tool not available" or with the coding-agent registry bridge.
//
// Receipts start at the pre-model gate and finish on the SESSION's terminal
// agent_end, not the core one. The core Agent ends a run before AgentSession
// decides whether to continue the same host request (overflow compaction and
// retry, a queued steer or follow-up that arrived after the last queue poll).
// AgentSession re-emits that end with isTerminal: false; only the terminal
// end may settle the request.
function bindSdkDispatch(opts: OmpSessionOptions) {
  if (!isAbsolute(opts.cwd)) throw new Error("OMP session cwd must be absolute");
  const handlers = opts.cursorExecHandlers ?? cursorExecHandlers(
    () => opts.cwd,
    () => toolResultBound(opts.agent.state.model?.contextWindow ?? 0),
    () => opts.policy?.(),
  );
  const inner = opts.agent.streamFn.bind(opts.agent);
  const measured = measureDispatch(() => opts.usage.durable ? opts.usage : undefined, opts.usageAttribution, inner);
  opts.agent.streamFn = (model, context, options) => measured(model, context, {
    ...options,
    cursorExecHandlers: handlers,
  });
  const open = new Set<string>();
  let turns = 0;
  opts.agent.addBeforeModelCall(context => {
    opts.receipts.assertHealthy();
    const ids: string[] = [];
    for (const message of context.messages) {
      const id = (message as ReceiptMessage).claudiaRequest?.request_id;
      if (!id) continue;
      const state = opts.receipts.query(id).state;
      if (state === "accepted" || state === "started") ids.push(id);
    }
    if (ids.length === 0) return;
    const turnID = opts.turnId?.() || `sdk-turn-${++turns}`;
    const fresh = ids.filter(id => !opts.receipts.query(id).turn_ids.includes(turnID));
    opts.receipts.start(ids, turnID);
    for (const id of ids) open.add(id);
    for (const id of fresh) opts.onReceipt?.(id, "started");
  });
  const settleDefault = (messages: readonly AgentMessage[]) => {
    const assistants = messages as { role?: string; stopReason?: string; errorMessage?: string }[];
    const aborted = assistants.some(message => message.role === "assistant" && message.stopReason === "aborted");
    const failed = assistants.some(message => message.role === "assistant" && (message.stopReason === "error" || Boolean(message.errorMessage)));
    return aborted ? { state: "cancelled" as const, reason: "cancelled", cause: "cancellation" as const }
      : failed ? { state: "failed" as const, reason: "provider_error", cause: "failure" as const }
      : { state: "completed" as const };
  };
  return (session: AgentSession) => session.subscribe(event => {
    if (event.type !== "agent_end" || (event as { isTerminal?: boolean }).isTerminal === false) return;
    const ids = [...open].filter(id => opts.receipts.query(id).state === "started");
    if (ids.length === 0) { open.clear(); return; }
    const messages = (event as { messages?: AgentMessage[] }).messages ?? [];
    const verdict = (opts.settle ?? settleDefault)(messages);
    if (!verdict) return;
    open.clear();
    opts.receipts.finish(ids, verdict.state, verdict.reason, verdict.cause);
    for (const id of ids) opts.onReceipt?.(id, "finished");
  });
}

export async function createOmpSession(opts: OmpSessionOptions) {
  if (!isAbsolute(opts.stateDir)) throw new Error("OMP session stateDir must be absolute");
  if (typeof opts.sideStreamFn !== "function") throw new Error("OMP session requires an observed sideStreamFn");
  if (typeof opts.nativeCompactionOptions !== "function") throw new Error("OMP session requires observed native compaction options");
  if (opts.agent.state.isStreaming) throw new Error("OMP session cannot adopt a running Agent");
  if (!(opts.receipts instanceof ReceiptJournal)) throw new Error("OMP session requires the production receipt journal");
  if (!(opts.usage instanceof UsageJournal)) throw new Error("OMP session requires the production usage journal");
  if (typeof opts.usageAttribution !== "function") throw new Error("OMP session requires usage attribution");
  const preserveSeatLoop = opts.preserveSeatLoop === true;
  const sdkRoot = opts.sdkRoot === undefined ? opts.stateDir : opts.sdkRoot;
  if (sdkRoot !== null && !isAbsolute(sdkRoot)) throw new Error("OMP session sdkRoot must be absolute");
  if (!preserveSeatLoop && sdkRoot !== null) for (const dir of [getAgentDir(), getBaseConfigRoot()]) {
    const suffix = relative(sdkRoot, dir);
    if (suffix === ".." || suffix.startsWith(`..${sep}`) || isAbsolute(suffix)) {
      throw new Error(`OMP SDK directory is outside the explicit state root: ${dir}`);
    }
  }
  for (const key of Object.keys(opts.settings ?? {})) {
    if (processGlobalSettingKeys.includes(key)) throw new Error(`OMP setting ${key} is process-wide in the SDK; a seat may not set it`);
  }
  const attachSettle = preserveSeatLoop ? undefined : bindSdkDispatch(opts);
  const settings = Settings.isolated({
    // Even ignoreLocalModelConfig registries add these implicit keyless
    // endpoints; none is an authenticated broker-selected compaction route.
    disabledProviders: ["local", "web", "ollama", "llama.cpp", "lm-studio"],
    "memory.backend": "off",
    "memories.enabled": false,
    "autolearn.enabled": false,
    "advisor.enabled": false,
    "prewalk.enabled": false,
    "async.enabled": false,
    "goal.enabled": false,
    "magicKeywords.enabled": false,
    "recap.enabled": false,
    "title.refreshOnReplan": false,
    // Outer session retries require service admission policy. Provider-level
    // retries remain owned and observed by the injected transport adapters.
    "retry.enabled": false,
    "retry.modelFallback": false,
    "retry.usageAwareFallback": false,
    "retry.waitForUsageReset": false,
    "codexResets.autoRedeem": false,
    "claudeResets.autoRedeem": false,
    // The seat's Maintenance pass remains the compactor when the seat owns
    // the loop. A second SDK compactor would fold the same turn twice.
    "compaction.enabled": !preserveSeatLoop,
    "compaction.midTurnEnabled": !preserveSeatLoop,
    "compaction.asyncEnabled": false,
    "compaction.idleEnabled": false,
    "compaction.experimentalContextManagement": false,
    // The SDK default tries snapcompact (bitmap archives) and handoff (a
    // handoff document plus an automatic continuation turn) before a
    // summary. A host request must not gain a turn it did not ask for.
    "compaction.methodOrder": ["remote", "soft"],
    ...opts.settings,
  });
  guardProcessGlobalSettings(settings);
  const noAuxiliaryFetch: typeof fetch = async () => {
    throw new Error("Claudia does not permit auxiliary SDK network discovery or credential requests");
  };
  const auth = await AuthStorage.create(":memory:", {
    allowEnvironmentCredentials: false,
    usageProviderResolver: () => undefined,
    usageFetch: noAuxiliaryFetch,
    configValueResolver: async () => undefined,
  });
  // Session preflight, native compaction and the agent loop each read a key
  // independently. Refresh the active provider from the broker on those reads
  // so a revoked token cannot remain authorized from the copy taken at startup.
  const syncToken = (provider: string): string => {
    const token = opts.tokenFor(provider);
    if (!token?.trim()) {
      auth.removeRuntimeApiKey(provider);
      throw new Error(`broker supplied no credential for ${provider}`);
    }
    auth.setRuntimeApiKey(provider, token);
    return token;
  };
  try {
    if (!opts.agent.state.model) throw new Error("OMP session requires an explicit model");
    syncToken(opts.agent.state.model.provider);
    const modelRegistry = new ModelRegistry(auth, join(opts.stateDir, "models.yml"), {
      ignoreLocalModelConfig: true,
      settings,
      cacheDbPath: join(opts.stateDir, "models.db"),
      fetch: noAuxiliaryFetch,
    });
    // ModelRegistry installs the CLI config-value resolver in its constructor.
    // This private registry accepts only broker runtime tokens, never shell or
    // local configuration credential helpers.
    auth.setConfigValueResolver(async () => undefined);
    // A provider token makes every bundled sibling look available. The SDK's
    // compaction and promotion chains would then call a different catalog model
    // at its own endpoint. The host's current model is the only authorized target.
    const listAvailable = modelRegistry.getAvailable.bind(modelRegistry);
    modelRegistry.getAvailable = (kind) => {
      const models = listAvailable(kind);
      const current = opts.agent.state.model;
      if (!current) return models;
      const selected = models.filter(model => model.provider === current.provider && model.id === current.id);
      return selected.length > 0 ? selected : [current];
    };
    const refreshActive = (provider: string) => {
      if (opts.agent.state.model?.provider !== provider) return;
      const token = opts.tokenFor(provider);
      if (!token?.trim()) {
        auth.removeRuntimeApiKey(provider);
        return;
      }
      auth.setRuntimeApiKey(provider, token);
    };
    const readModelKey = modelRegistry.getApiKey.bind(modelRegistry);
    modelRegistry.getApiKey = async (model, sessionId, options) => {
      refreshActive(model.provider);
      return readModelKey(model, sessionId, options);
    };
    const readProviderKey = modelRegistry.getApiKeyForProvider.bind(modelRegistry);
    modelRegistry.getApiKeyForProvider = async (provider, sessionId, options) => {
      refreshActive(provider);
      return readProviderKey(provider, sessionId, options);
    };
    // The agent loop does not consult the registry key. One broker callback
    // authorizes both paths; a stale callback captured at construction cannot.
    // A seat-owned loop keeps the callback that counts attempts and replays
    // the rotated token. Replacing it breaks auth recovery.
    const previousStream = opts.agent.streamFn;
    const previousKey = opts.agent.getApiKey;
    const previousPrepare = opts.agent.prepareQueuedMessages;
    if (!preserveSeatLoop) {
      opts.agent.getApiKey = (model) => syncToken(model.provider);
      // Direct construction does not perform the SDK factory's restore step.
      // The canonical manager owns context, including compaction and branching.
      opts.agent.replaceMessages(opts.sessionManager.buildSessionContext().messages);
    }
    const session = new AgentSession({
      agent: opts.agent,
      sessionManager: opts.sessionManager,
      settings,
      modelRegistry,
      autoReadFileMentions: false,
      memoryEnabled: false,
      disableExtensionDiscovery: true,
      scoutAllowedBySpawnPolicy: false,
      skills: [],
      skillsReloadable: false,
      promptTemplates: [],
      slashCommands: [],
      customCommands: [],
      advisorConfigs: [],
      builtInToolNames: [],
      mcpManagerToolNames: [],
      toolRegistry: new Map(opts.agent.state.tools.map(tool => [tool.name, tool])),
      sideStreamFn: opts.sideStreamFn,
      nativeCompactionOptions: opts.nativeCompactionOptions,
      convertToLlm: convertOmpMessages,
      ...(opts.beforeCompact ? { extensionRunner: compactionHookRunner(opts.beforeCompact) as never } : {}),
    });
    const detachSettle = attachSettle?.(session);
    if (preserveSeatLoop) {
      // AgentSession installs its own queue preparer, which rebuilds user
      // text and drops the host request on the message. The seat's steering
      // order and receipt identity depend on the preparer it already had.
      opts.agent.streamFn = previousStream;
      opts.agent.getApiKey = previousKey;
      opts.agent.prepareQueuedMessages = previousPrepare;
    }
    let disposal: Promise<void> | undefined;
    return {
      session,
      settings,
      syncToken,
      getApiKey: (model: Model) => syncToken(model.provider),
      dispose: () => disposal ??= (async () => {
        try { detachSettle?.(); await session.dispose(); } finally { auth.close(); }
      })(),
    };
  } catch (error) {
    auth.close();
    throw error;
  }
}

// The production seat keeps calling Agent.prompt with the host message.
// AgentSession is adopted beside that loop. It does not submit text, replace
// the stream, or bookend receipts; the seat already does those.
export function adoptSeatSession(opts: {
  agent: Agent;
  cwd: string;
  stateDir: string;
  tokenFor: (provider: string) => string | undefined;
  receipts: ReceiptJournal;
  usage: UsageJournal;
  usageAttribution: () => Attribution;
}) {
  if (!isAbsolute(opts.cwd)) throw new Error("OMP seat cwd must be absolute");
  mkdirSync(opts.stateDir, { recursive: true });
  return createOmpSession({
    agent: opts.agent,
    sessionManager: SessionManager.inMemory(opts.cwd),
    stateDir: opts.stateDir,
    cwd: opts.cwd,
    tokenFor: opts.tokenFor,
    sideStreamFn: streamSimple,
    nativeCompactionOptions: observedNativeCompaction(opts.usage, opts.usageAttribution, fetch),
    receipts: opts.receipts,
    usage: opts.usage,
    usageAttribution: opts.usageAttribution,
    preserveSeatLoop: true,
  });
}
