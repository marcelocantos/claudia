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

export type SeatEmit = (ev: SeatEvent) => void;
export type SeatCallTool = (callId: string, name: string, args: string) => Promise<string>;

export type SeatAgent = {
  prompt: (text: string) => Promise<void>;
  steer: (text: string) => void;
  abort: () => void;
  setModel: (provider: string, model: string) => void;
  setToken: (token: string) => void;
  setCwd: (cwd: string) => void;
  snapshot: () => unknown;
  // rebind points events and tool calls at the connection that just
  // loaded the seat. A broker restart dials a new socket; the Agent
  // stays, and the new socket has to hear it (🎯T868).
  rebind: (emit: SeatEmit, callTool: SeatCallTool) => void;
};

export function createSeatAgent(opts: {
  provider: string;
  model: string;
  token: string;
  cwd: string;
  emit: SeatEmit;
  callTool: SeatCallTool;
}): SeatAgent {
  let token = opts.token;
  let cwd = opts.cwd;
  const sink = { emit: opts.emit, callTool: opts.callTool };
  const model = resolveModel(opts.provider, opts.model);
  const agent = new Agent({
    initialState: {
      systemPrompt: ["You are a coding agent hosted by Claudia."],
      model,
    },
    cwd,
    cwdResolver: () => cwd || undefined,
    getApiKey: async () => token,
    resolveFallbackTool: (name: string) => {
      if (!name.startsWith("jevons_")) return undefined;
      return jevonsTool(name, (id, toolName, args) => sink.callTool(id, toolName, args));
    },
  });

  agent.subscribe((event: { type?: string; assistantMessageEvent?: { type?: string; delta?: string } }) => {
    if (event.type === "message_update" && event.assistantMessageEvent?.type === "text_delta") {
      sink.emit({ type: "text", text: event.assistantMessageEvent.delta ?? "" });
    }
  });

  return {
    prompt: async (text: string) => {
      await agent.prompt(text);
      sink.emit({ type: "turn_end", snapshot: agent.state });
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
    setCwd: (next: string) => {
      if (next) cwd = next;
    },
    snapshot: () => agent.state,
    rebind: (emit, callTool) => {
      sink.emit = emit;
      sink.callTool = callTool;
    },
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
