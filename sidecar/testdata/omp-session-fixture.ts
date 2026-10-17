// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import assert from "node:assert/strict";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import type { CursorExecHandlers } from "@oh-my-pi/pi-ai";
// Block unexpected auxiliary network work before loading the SDK.
globalThis.fetch = (async () => { throw new Error("unexpected fixture network request"); }) as typeof fetch;
const { Agent } = await import("@oh-my-pi/pi-agent-core");
const { AuthStorage, streamSimple } = await import("@oh-my-pi/pi-ai");
const { createMockModel, registerMockApi } = await import("@oh-my-pi/pi-ai/providers/mock");
const { AgentSession } = await import("@oh-my-pi/pi-coding-agent/session/agent-session");
const { SessionManager } = await import("@oh-my-pi/pi-coding-agent/session/session-manager");
const { parseSessionContent } = await import("@oh-my-pi/pi-coding-agent/session/session-loader");
const { createOmpSession, convertOmpMessages } = await import("../omp-session.ts");
const { OMPJournal } = await import("../omp-journal.ts");
const { ReceiptJournal, requestFactType } = await import("../receipts.ts");

const root = process.argv[2];
const cwd = join(root, "work");
mkdirSync(cwd);
const outside = join(root, "outside.txt");
const secret = "OUTSIDE_FILE_SENTINEL_6c89b";
writeFileSync(outside, secret);
registerMockApi();
const contexts: unknown[] = [];
let expectedDispatch: string | undefined;
const model = createMockModel({
  provider: "fixture-provider",
  id: "fixture-model",
  contextWindow: 200_000,
  handler: context => {
    if (expectedDispatch) {
      // This is the actual SDK transport boundary: independently read disk
      // before the mock response. No in-memory receipt substitutes for it.
      const persisted = parseSessionContent(readFileSync(journal.store.path, "utf8"));
      const facts = persisted.entries.filter(entry => entry.type === "custom" && entry.customType === requestFactType)
        .map(entry => entry.data as { receipt: { request_id: string; state: string } })
        .filter(fact => fact.receipt.request_id === expectedDispatch);
      assert.deepEqual(facts.map(fact => fact.receipt.state), ["accepted", "started"]);
      assert.ok(context.messages.some(message => "claudiaRequest" in message &&
        (message.claudiaRequest as { request_id: string }).request_id === expectedDispatch));
    }
    contexts.push(structuredClone(context));
    return { content: ["OK"], usage: { input: 10, output: 1 } };
  },
});
let token: string | undefined = "fixture-token-one";
const makeAgent = () => new Agent({
  initialState: { model, systemPrompt: ["Reply OK."], tools: [] },
  cwd,
  getApiKey: () => token,
  convertToLlm: convertOmpMessages,
});
let dispatchedHandlers: CursorExecHandlers | undefined;
const agent = new Agent({
  initialState: { model, systemPrompt: ["Reply OK."], tools: [] },
  cwd,
  getApiKey: () => token,
  convertToLlm: convertOmpMessages,
  streamFn: (m, c, o) => {
    dispatchedHandlers = o?.cursorExecHandlers;
    return streamSimple(m, c, o);
  },
});
const journalOptions = { root: join(root, "sessions"), seat: "fixture-seat", session: "fixture-generation", cwd };
const journal = await OMPJournal.open(journalOptions);
const manager = journal.manager;
const receipts = new ReceiptJournal(journal);
let observedRequests: unknown[] = [];
agent.addBeforeModelCall(context => {
  observedRequests = context.messages.filter(message => "claudiaRequest" in message)
    .map(message => message.claudiaRequest);
});
const tokens = new Map<string, string>();
const tokenFor = (provider: string) => provider === model.provider ? token : tokens.get(provider);
const nativeCompactionOptions = () => {
  throw new Error("native compaction is not part of this fixture");
};
const policy = () => ({ mode: "workspace-write" as const, protected: [join(cwd, ".git")] });
const adapter = await createOmpSession({
  agent, sessionManager: manager, stateDir: root, tokenFor, sideStreamFn: streamSimple, nativeCompactionOptions,
  cwd, policy, receipts,
});
let raw: InstanceType<typeof AgentSession> | undefined;
try {
  await assert.rejects(createOmpSession({ agent: makeAgent(), sessionManager: SessionManager.inMemory(cwd),
    stateDir: join(root, "wrong-root"), tokenFor, sideStreamFn: streamSimple, nativeCompactionOptions,
    cwd, receipts }), /outside the explicit state root/);
  await assert.rejects(createOmpSession({ agent: makeAgent(), sessionManager: SessionManager.inMemory(cwd),
    stateDir: root, tokenFor, sideStreamFn: undefined!, nativeCompactionOptions }), /requires an observed sideStreamFn/);
  await assert.rejects(createOmpSession({ agent: makeAgent(), sessionManager: SessionManager.inMemory(cwd),
    stateDir: root, tokenFor, sideStreamFn: streamSimple, nativeCompactionOptions: undefined! }), /requires observed native compaction options/);
  const auth = adapter.session.modelRegistry.authStorage;
  const hostileProviders = ["anthropic", "xai", "xai-oauth", "amazon-bedrock", "bedrock-mantle"];
  const assertNoCredential = async (provider: string) => {
    assert.equal(auth.hasAuth(provider), false, provider);
    assert.equal(auth.hasConcreteAuth(provider), false, provider);
    assert.equal(auth.hasResolvableAuth(provider), false, provider);
    assert.equal(auth.hasNonEnvCredential(provider), false, provider);
    assert.equal(auth.getCredentialOrigin(provider), undefined, provider);
    assert.equal(auth.describeCredentialSource(provider), undefined, provider);
    assert.equal(auth.hasOAuth(provider), false, provider);
    assert.equal(await auth.getApiKey(provider), undefined, provider);
    assert.equal(await auth.peekApiKey(provider), undefined, provider);
    assert.equal(await adapter.session.modelRegistry.getApiKeyForProvider(provider), undefined, provider);
    assert.deepEqual(adapter.session.modelRegistry.getAvailableForProviders(new Set([provider])), [], provider);
  };
  for (const provider of hostileProviders) await assertNoCredential(provider);
  for (const provider of ["local", "web", "ollama", "llama.cpp", "lm-studio"]) {
    await assertNoCredential(provider);
  }
  // The SDK option is additive: ordinary AuthStorage still sees fake ambient
  // credentials. This control cannot read a disk credential store.
  const defaultAuth = await AuthStorage.create(":memory:");
  try { assert.equal(await defaultAuth.getApiKey("anthropic"), "fixture-ambient-anthropic"); }
  finally { defaultAuth.close(); }
  tokens.set("anthropic", "fixture-broker-anthropic");
  adapter.syncToken("anthropic");
  assert.equal(auth.hasConcreteAuth("anthropic"), true);
  assert.ok(adapter.session.modelRegistry.getAvailableForProviders(new Set(["anthropic"])).length > 0);
  assert.equal(await auth.getApiKey("anthropic"), "fixture-broker-anthropic");
  tokens.delete("anthropic");
  assert.throws(() => adapter.syncToken("anthropic"), /broker supplied no credential/);
  await assertNoCredential("anthropic");
  assert.equal(adapter.settings.get("retry.enabled"), false);
  const details = { request_id: "request-A", attempt_id: "attempt-one" };
  receipts.accept(`Read @${outside}`, { ...details, session_id: journal.store.session }, "prompt");
  expectedDispatch = details.request_id;
  assert.equal(await adapter.session.promptCustomMessage({
    customType: "claudia-request", content: `Read @${outside}`, display: true, attribution: "user", details,
  }), true);
  assert.equal(receipts.query(details.request_id).state, "completed");
  assert.deepEqual(observedRequests, [details]);
  assert.equal(contexts.length, 1);
  assert.equal(JSON.stringify(contexts[0]).includes(secret), false);
  assert.equal(JSON.stringify(agent.state.messages).includes(secret), false);
  assert.equal(JSON.stringify(manager.getEntries()).includes(secret), false);
  assert.match(JSON.stringify(contexts[0]), /Read @/);
  assert.equal(agent.state.tools.length, 0);
  assert.equal(adapter.settings.get("memory.backend"), "off");
  assert.equal(adapter.settings.get("memories.enabled"), false);
  assert.equal(adapter.settings.get("advisor.enabled"), false);
  assert.equal(adapter.settings.get("prewalk.enabled"), false);
  assert.equal(adapter.settings.get("async.enabled"), false);
  assert.equal(adapter.settings.get("goal.enabled"), false);
  assert.equal(adapter.settings.get("retry.enabled"), false);
  const handlers = dispatchedHandlers;
  assert.ok(handlers, "SDK dispatch installed no Cursor exec handlers");
  for (const name of ["read", "ls", "grep", "write", "delete", "shell", "piRead", "piBash", "piEdit", "piWrite", "piGrep", "piFind", "piLs"] as const) {
    assert.equal(typeof handlers[name], "function", name);
  }
  assert.equal(handlers.shellStream, undefined);
  const { resolveExecHandler } = await import("@oh-my-pi/pi-ai/providers/cursor");
  const missing = await resolveExecHandler(
    { path: "absent.txt", toolCallId: "missing" },
    undefined,
    undefined,
    () => "ok",
    (reason: string) => reason,
    (error: string) => error,
    { toolCallId: "missing", toolName: "read" },
  );
  assert.equal(missing.execResult, "Tool not available");
  assert.match(JSON.stringify(missing.toolResult), /Tool not available/);
  mkdirSync(join(cwd, ".git"));
  const wrote = await handlers.write({ path: "note.txt", fileText: "sdk-handler-body\n", toolCallId: "w1" });
  assert.equal(wrote.isError, false);
  assert.equal(readFileSync(join(cwd, "note.txt"), "utf8"), "sdk-handler-body\n");
  const readBack = await handlers.read({ path: "note.txt", toolCallId: "r1" });
  assert.equal(readBack.isError, false);
  assert.equal(JSON.stringify(readBack).includes("Tool not available"), false);
  assert.match(JSON.stringify(readBack), /sdk-handler-body/);
  const listed = await handlers.ls({ path: ".", toolCallId: "ls1" });
  assert.match(JSON.stringify(listed), /note.txt/);
  const found = await handlers.grep({ pattern: "sdk-handler-body", path: ".", toolCallId: "g1" });
  assert.match(JSON.stringify(found), /note.txt/);
  const ranged = await handlers.piRead({ toolCallId: "pr", args: { path: "note.txt", offset: 1, limit: 1 } });
  assert.equal(ranged.isError, false);
  assert.match(JSON.stringify(ranged), /sdk-handler-body/);
  const edited = await handlers.piEdit({ toolCallId: "pe", args: { path: "note.txt", edits: [{ oldText: "body", newText: "edited" }] } });
  assert.equal(edited.isError, false);
  assert.match(readFileSync(join(cwd, "note.txt"), "utf8"), /sdk-handler-edited/);
  const piWrote = await handlers.piWrite({ toolCallId: "pw", args: { path: "pi.txt", content: "pi-write\n" } });
  assert.equal(piWrote.isError, false);
  const piGrep = await handlers.piGrep({ toolCallId: "pg", args: { pattern: "pi-write", path: "." } });
  assert.match(JSON.stringify(piGrep), /pi.txt/);
  const piFind = await handlers.piFind({ toolCallId: "pf", args: { pattern: "pi.txt" } });
  assert.match(JSON.stringify(piFind), /pi.txt/);
  const piLs = await handlers.piLs({ toolCallId: "pl", args: {} });
  assert.match(JSON.stringify(piLs), /pi.txt/);
  const piBash = await handlers.piBash({ toolCallId: "pb", args: { command: "printf pi-bash" } });
  assert.equal(piBash.isError, false);
  assert.match(JSON.stringify(piBash), /pi-bash/);
  const ran = await handlers.shell({ command: "pwd", toolCallId: "s1" });
  assert.equal(ran.isError, false);
  assert.match(JSON.stringify(ran), /work/);
  const removed = await handlers.delete({ path: "note.txt", toolCallId: "d1" });
  assert.equal(removed.isError, false);
  const protectedWrite = await handlers.write({ path: ".git/HEAD", fileText: "no\n", toolCallId: "deny" });
  assert.equal(protectedWrite.isError, true);
  assert.match(JSON.stringify(protectedWrite), /protected/);
  const escaped = await handlers.read({ path: outside, toolCallId: "esc" });
  assert.equal(escaped.isError, true);
  assert.match(JSON.stringify(escaped), /escapes working directory/);
  // Repeated payloads are distinct requests. Identity follows the custom
  // message itself, not text matching or the newest queue slot.
  const second = { request_id: "request-B", attempt_id: "attempt-two" };
  receipts.accept(`Read @${outside}`, { ...second, session_id: journal.store.session }, "prompt");
  expectedDispatch = second.request_id;
  await adapter.session.promptCustomMessage({ customType: "claudia-request",
    content: `Read @${outside}`, display: true, attribution: "user", details: second });
  assert.equal(receipts.query(second.request_id).state, "completed");
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
  expectedDispatch = undefined;
  await raw.promptCustomMessage({ customType: "fixture", content: `Read @${outside}`,
    display: true, attribution: "user", details: {} });
  assert.equal(contexts.length, 3);
  assert.equal(JSON.stringify(contexts[2]).includes(secret), true);
} finally {
  await raw?.dispose();
  await adapter.dispose();
  await journal.close();
}
// Restoration is exercised through the actual canonical disk journal, not a
// hand-created message array. The Agent constructor starts without history.
const reopened = await OMPJournal.open({ ...journalOptions, requireResume: true });
const restoredAgent = makeAgent();
assert.equal(restoredAgent.state.messages.length, 0);
const restoredReceipts = new ReceiptJournal(reopened);
const restored = await createOmpSession({ agent: restoredAgent, sessionManager: reopened.manager,
  stateDir: root, tokenFor, sideStreamFn: streamSimple, nativeCompactionOptions,
  cwd, policy, receipts: restoredReceipts });
try {
  assert.equal(new ReceiptJournal(reopened).query("request-A").state, "completed");
  assert.equal(new ReceiptJournal(reopened).query("request-B").state, "completed");
  assert.equal(restoredAgent.state.messages.filter(message => message.role === "assistant").length, 2);
  const restoredIDs = convertOmpMessages(restoredAgent.state.messages)
    .filter(message => "claudiaRequest" in message).map(message => message.claudiaRequest);
  assert.deepEqual(restoredIDs, [
    { request_id: "request-A", attempt_id: "attempt-one" },
    { request_id: "request-B", attempt_id: "attempt-two" },
  ]);
  token = undefined;
  assert.throws(() => restored.syncToken(model.provider), /broker supplied no credential/);
  assert.equal(await restored.session.modelRegistry.getApiKey(model), undefined);
  await assert.rejects(restored.session.promptCustomMessage({ customType: "claudia-request",
    content: "must not dispatch", display: true, attribution: "user",
    details: { request_id: "revoked", attempt_id: "revoked-attempt" } }), /API key|credential/i);
  assert.equal(contexts.length, 3);
} finally {
  await restored.dispose();
  await reopened.close();
}
console.log("OMP session fixture passed");
