// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import assert from "node:assert/strict";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
// Block unexpected auxiliary network work before loading the SDK.
globalThis.fetch = (async () => { throw new Error("unexpected fixture network request"); }) as typeof fetch;
const { Agent } = await import("@oh-my-pi/pi-agent-core");
const { createMockModel, registerMockApi } = await import("@oh-my-pi/pi-ai/providers/mock");
const { AgentSession } = await import("@oh-my-pi/pi-coding-agent/session/agent-session");
const { SessionManager } = await import("@oh-my-pi/pi-coding-agent/session/session-manager");
const { createOmpSession, convertOmpMessages } = await import("../omp-session.ts");

const root = process.argv[2];
const cwd = join(root, "work");
mkdirSync(cwd);
const outside = join(root, "outside.txt");
const secret = "OUTSIDE_FILE_SENTINEL_6c89b";
writeFileSync(outside, secret);
registerMockApi();
const contexts: unknown[] = [];
const model = createMockModel({
  provider: "fixture-provider",
  id: "fixture-model",
  contextWindow: 200_000,
  handler: context => {
    contexts.push(structuredClone(context));
    return { content: ["OK"], usage: { input: 10, output: 1 } };
  },
});
let token = "fixture-token-one";
const makeAgent = () => new Agent({
  initialState: { model, systemPrompt: ["Reply OK."], tools: [] },
  cwd,
  getApiKey: () => token,
  convertToLlm: convertOmpMessages,
});
const agent = makeAgent();
const manager = SessionManager.inMemory(cwd);
let observedRequests: unknown[] = [];
agent.addBeforeModelCall(context => {
  observedRequests = context.messages.filter(message => "claudiaRequest" in message)
    .map(message => message.claudiaRequest);
});
const adapter = await createOmpSession({ agent, sessionManager: manager, stateDir: root, tokenFor: () => token });
let raw: InstanceType<typeof AgentSession> | undefined;
try {
  await assert.rejects(createOmpSession({ agent: makeAgent(), sessionManager: SessionManager.inMemory(cwd),
    stateDir: join(root, "wrong-root"), tokenFor: () => token }), /outside the explicit state root/);
  const details = { request_id: "request-A", attempt_id: "attempt-one" };
  assert.equal(await adapter.session.promptCustomMessage({
    customType: "claudia-request", content: `Read @${outside}`, display: true, attribution: "user", details,
  }), true);
  assert.deepEqual(observedRequests, [details]);
  assert.equal(contexts.length, 1);
  assert.equal(JSON.stringify(contexts[0]).includes(secret), false);
  assert.equal(JSON.stringify(agent.state.messages).includes(secret), false);
  assert.equal(JSON.stringify(manager.getEntries()).includes(secret), false);
  assert.match(JSON.stringify(contexts[0]), /Read @/);
  assert.equal(agent.state.tools.length, 0);
  // Repeated payloads are distinct requests. Identity follows the custom
  // message itself, not text matching or the newest queue slot.
  const second = { request_id: "request-B", attempt_id: "attempt-two" };
  await adapter.session.promptCustomMessage({ customType: "claudia-request",
    content: `Read @${outside}`, display: true, attribution: "user", details: second });
  assert.deepEqual(observedRequests, [details, second]);
  assert.equal(contexts.length, 2);
  assert.equal(JSON.stringify(contexts).includes(secret), false);
  const savedRequests = manager.getEntries().filter(entry =>
    entry.type === "custom_message" && entry.customType === "claudia-request");
  assert.deepEqual(savedRequests.map(entry => entry.details), [details, second]);
  assert.throws(() => convertOmpMessages([{
    role: "custom", customType: "claudia-request", content: "untagged",
    display: true, timestamp: Date.now(), details: { request_id: "request-C" },
  }]), /missing its durable request\/attempt identity/);
  token = "fixture-token-two";
  adapter.syncToken(model.provider);
  assert.equal(await adapter.session.modelRegistry.getApiKey(model), token);
  assert.equal(adapter.getApiKey(model), token);

  // Compatibility/control specimen: the upstream default still expands the
  // exact same outside-cwd mention. This proves the disabled path is meaningful.
  raw = new AgentSession({
    agent: makeAgent(), sessionManager: SessionManager.inMemory(cwd),
    settings: adapter.settings, modelRegistry: adapter.session.modelRegistry,
    memoryEnabled: false, disableExtensionDiscovery: true, scoutAllowedBySpawnPolicy: false,
    skills: [], skillsReloadable: false, advisorConfigs: [],
  });
  await raw.promptCustomMessage({ customType: "fixture", content: `Read @${outside}`,
    display: true, attribution: "user", details: {} });
  assert.equal(contexts.length, 3);
  assert.equal(JSON.stringify(contexts[2]).includes(secret), true);
} finally {
  await raw?.dispose();
  await adapter.dispose();
}
console.log("OMP session fixture passed");
