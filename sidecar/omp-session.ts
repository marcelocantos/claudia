// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Claudia supplies transport, credentials and tool policy. OMP owns the session
// loop and context maintenance; this module does not run a second turn loop.
import type { Agent, AgentMessage, StreamFn } from "@oh-my-pi/pi-agent-core";
import { AuthStorage, type Model } from "@oh-my-pi/pi-ai";
import { copyPerCallContextMessage } from "@oh-my-pi/pi-ai/utils/block-symbols";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { AgentSession } from "@oh-my-pi/pi-coding-agent/session/agent-session";
import { convertToLlm } from "@oh-my-pi/pi-coding-agent/session/messages";
import type { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { getAgentDir, getBaseConfigRoot } from "@oh-my-pi/pi-utils/dirs";
import { isAbsolute, join, relative, sep } from "node:path";
import type { ReceiptMessage } from "./receipts.ts";

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
  sideStreamFn?: StreamFn;
}

export async function createOmpSession(opts: OmpSessionOptions) {
  if (!isAbsolute(opts.stateDir)) throw new Error("OMP session stateDir must be absolute");
  for (const dir of [getAgentDir(), getBaseConfigRoot()]) {
    const suffix = relative(opts.stateDir, dir);
    if (suffix === ".." || suffix.startsWith(`..${sep}`) || isAbsolute(suffix)) {
      throw new Error(`OMP SDK directory is outside the explicit state root: ${dir}`);
    }
  }
  const settings = Settings.isolated({
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
    "retry.enabled": true,
    "retry.modelFallback": false,
    "retry.usageAwareFallback": false,
    "retry.waitForUsageReset": false,
    "codexResets.autoRedeem": false,
    "claudeResets.autoRedeem": false,
    "compaction.enabled": true,
    "compaction.midTurnEnabled": true,
    "compaction.asyncEnabled": false,
    "compaction.idleEnabled": false,
    "compaction.experimentalContextManagement": false,
  });
  const noAuxiliaryFetch: typeof fetch = async () => {
    throw new Error("Claudia does not permit auxiliary SDK network discovery or credential requests");
  };
  const auth = await AuthStorage.create(":memory:", {
    usageProviderResolver: () => undefined,
    usageFetch: noAuxiliaryFetch,
    configValueResolver: async () => undefined,
  });
  // Session preflight and native compaction consult ModelRegistry independently
  // of Agent.getApiKey. Keep their private runtime credential copy synchronized
  // on broker refresh and before selecting a destination model.
  const syncToken = (provider: string): string => {
    const token = opts.tokenFor(provider);
    if (!token) throw new Error(`broker supplied no credential for ${provider}`);
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
      convertToLlm: convertOmpMessages,
    });
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
