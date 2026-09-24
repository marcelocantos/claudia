// Seat factory for the Oh My Pi sidecar (🎯T864.3).
// A prompt must call Agent.prompt on the pinned @oh-my-pi/pi-agent-core.

import { Agent, type AgentTool } from "@oh-my-pi/pi-agent-core";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";

export type SeatEvent = {
  type: string;
  text?: string;
  call_id?: string;
  name?: string;
  snapshot?: unknown;
};

export type SeatAgent = {
  prompt: (text: string) => Promise<void>;
  steer: (text: string) => void;
  abort: () => void;
  setModel: (provider: string, model: string) => void;
  setToken: (token: string) => void;
  snapshot: () => unknown;
};

export function createSeatAgent(opts: {
  provider: string;
  model: string;
  token: string;
  emit: (ev: SeatEvent) => void;
  callTool: (callId: string, name: string, args: string) => Promise<string>;
}): SeatAgent {
  let token = opts.token;
  const model = resolveModel(opts.provider, opts.model);
  const agent = new Agent({
    initialState: {
      systemPrompt: ["You are a coding agent hosted by Claudia."],
      model,
    },
    getApiKey: async () => token,
    resolveFallbackTool: (name: string) => {
      if (!name.startsWith("jevons_")) return undefined;
      return jevonsTool(name, opts.callTool);
    },
  });

  agent.subscribe((event: { type?: string; assistantMessageEvent?: { type?: string; delta?: string } }) => {
    if (event.type === "message_update" && event.assistantMessageEvent?.type === "text_delta") {
      opts.emit({ type: "text", text: event.assistantMessageEvent.delta ?? "" });
    }
  });

  return {
    prompt: async (text: string) => {
      await agent.prompt(text);
      opts.emit({ type: "turn_end", snapshot: agent.state });
    },
    steer: (text: string) => {
      agent.steer({
        role: "user",
        content: text,
        timestamp: Date.now(),
      });
    },
    abort: () => {
      agent.abort();
    },
    setModel: (provider: string, modelId: string) => {
      agent.setModel(resolveModel(provider, modelId));
    },
    setToken: (next: string) => {
      token = next;
    },
    snapshot: () => agent.state,
  };
}

function resolveModel(provider: string, model: string) {
  if (!model) {
    throw new Error("load requires a model id");
  }
  const found = getBundledModel(provider as never, model);
  if (!found) {
    throw new Error(`unknown model ${provider}/${model}`);
  }
  return found;
}

function jevonsTool(
  name: string,
  callTool: (callId: string, name: string, args: string) => Promise<string>,
): AgentTool {
  return {
    name,
    label: name,
    description: "Jevons host tool; execute calls back into Go",
    parameters: { type: "object" },
    execute: async (toolCallId: string, params: unknown) => {
      const result = await callTool(toolCallId, name, JSON.stringify(params ?? {}));
      return { content: [{ type: "text", text: result }], details: {} };
    },
  } as AgentTool;
}
