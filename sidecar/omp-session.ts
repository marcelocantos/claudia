// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Claudia supplies transport, credentials and tool policy. OMP owns the session
// loop and context maintenance; this module does not run a second turn loop.
import type { Agent, AgentMessage, StreamFn } from "@oh-my-pi/pi-agent-core";
import { AuthStorage, streamSimple, type Model } from "@oh-my-pi/pi-ai";
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
import { ReceiptJournal, type ReceiptMessage } from "./receipts.ts";
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
}

// The Agent stores Cursor exec handlers only at construction, and the loop
// copies that private field into the provider stream options. Overwrite the
// options on the way through so an SDK session cannot answer a Cursor frame
// with "Tool not available" or with the coding-agent registry bridge.
function bindSdkDispatch(opts: OmpSessionOptions) {
  if (!isAbsolute(opts.cwd)) throw new Error("OMP session cwd must be absolute");
  const handlers = cursorExecHandlers(
    () => opts.cwd,
    () => toolResultBound(opts.agent.state.model?.contextWindow ?? 0),
    () => opts.policy?.(),
  );
  const inner = opts.agent.streamFn.bind(opts.agent);
  const measured = measureDispatch(() => opts.usage, opts.usageAttribution, inner);
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
    opts.receipts.start(ids, `sdk-turn-${++turns}`);
    for (const id of ids) open.add(id);
  });
  opts.agent.subscribe(event => {
    if (event.type !== "agent_end") return;
    const messages = (event as { messages?: { role?: string; stopReason?: string; errorMessage?: string }[] }).messages ?? [];
    const aborted = messages.some(message => message.role === "assistant" && message.stopReason === "aborted");
    const failed = messages.some(message => message.role === "assistant" && (message.stopReason === "error" || Boolean(message.errorMessage)));
    const ids = [...open];
    open.clear();
    if (ids.length === 0) return;
    const state = aborted ? "cancelled" : failed ? "failed" : "completed";
    opts.receipts.finish(ids, state, aborted ? "cancelled" : failed ? "provider_error" : undefined,
      aborted ? "cancellation" : failed ? "failure" : undefined);
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
  if (!preserveSeatLoop) bindSdkDispatch(opts);
  if (!preserveSeatLoop) for (const dir of [getAgentDir(), getBaseConfigRoot()]) {
    const suffix = relative(opts.stateDir, dir);
    if (suffix === ".." || suffix.startsWith(`..${sep}`) || isAbsolute(suffix)) {
      throw new Error(`OMP SDK directory is outside the explicit state root: ${dir}`);
    }
  }
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
  });
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
    });
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
        try { await session.dispose(); } finally { auth.close(); }
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
