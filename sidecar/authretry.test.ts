import { expect, test } from "bun:test";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent, type SeatEvent } from "./seat.ts";
import { AuthRetry, authRefusal, tokenFingerprint } from "./authretry.ts";
registerMockApi();
const refusal = "401 OAuth access token has expired";

function fixture(reply: string, always = false) {
  const events: SeatEvent[] = [];
  const mock = createMockModel({ handler: () => mock.calls.length === 1 || always
    ? { stopReason: "error", errorMessage: refusal } : { content: ["recovered"] } });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "failed", cwd: process.cwd(), modelOverride: mock,
    emit: (e) => { events.push(e); if (e.type === "auth_retry") agent.authRetry({ ...e, token: reply, expires_at: Date.now() + 60000 }); }, callTool: async () => "" });
  return { agent, mock, events };
}

test("T169 different delivered valid token replays once without intermediate terminal error", async () => {
  const { agent, mock, events } = fixture("replacement");
  await agent.prompt("original", { turn_id: "owner-turn" });
  expect(mock.calls.length).toBe(2);
  expect(events.filter(e => e.type === "turn_end").map(e => e.error)).toEqual([undefined]);
  expect(JSON.stringify(mock.calls[1].context.messages)).toContain("original");
});
test("T169 unchanged token and sign-in failure never replay", async () => {
  for (const token of ["failed", ""]) {
    const { agent, mock, events } = fixture(token);
    await agent.prompt("original");
    expect(mock.calls.length).toBe(1);
    expect(events.at(-1)?.error).toBe(refusal);
  }
});
test("T169 second refusal is terminal", async () => {
  const { agent, mock, events } = fixture("replacement", true);
  await agent.prompt("original");
  expect(mock.calls.length).toBe(2);
  expect(events.filter(e => e.type === "auth_retry").length).toBe(1);
  expect(events.at(-1)?.error).toBe(refusal);
});
test("T169 handshake fails closed on expiry, missing delivery and wrong identity", async () => {
  for (const mode of ["expired", "missing", "wrong-turn", "wrong-request", "unknown-expiry"]) {
    let retry: AuthRetry;
    retry = new AuthRetry(e => {
      expect(e.failed_token).toBe(tokenFingerprint("old"));
      if (mode === "missing") return;
      retry.receive({ ...e, token: "new", turn_id: mode === "wrong-turn" ? "other" : e.turn_id,
        request_id: mode === "wrong-request" ? "other" : e.request_id,
        expires_at: mode === "unknown-expiry" ? undefined : Date.now() + (mode === "expired" ? -1 : 60000) });
    }, 5);
    expect(await retry.recover("turn", "old", refusal)).toBe("");
  }
});
test("T169 non-auth errors never request recovery", () => {
  for (const error of ["429 rate limit", "503 server unavailable", "prompt too long", ""]) expect(authRefusal(error)).toBe(false);
});

test("T169 text, built-in and host tool effects prohibit replay", async () => {
  for (const effect of ["text", "Bash", "host"]) {
    let calls = 0;
    const events: SeatEvent[] = [];
    const mock = createMockModel({ handler: () => {
      calls++;
      if (calls === 1) return effect === "text"
        ? { content: ["already visible"], stopReason: "error", errorMessage: refusal }
        : { content: [{ type: "toolCall", name: effect, arguments: effect === "Bash" ? { command: "true" } : {} }] };
      return { stopReason: "error", errorMessage: refusal };
    } });
    const agent = createSeatAgent({ provider: "mock", model: "mock", token: "failed", cwd: process.cwd(), modelOverride: mock,
      emit: e => events.push(e), tools: [{ name: "host", input_schema: { type: "object" } }], callTool: async () => "effect" });
    await agent.prompt("work");
    expect(events.filter(e => e.type === "auth_retry")).toEqual([]);
    expect(events.at(-1)?.error).toBe(refusal);
  }
});

test("T169 recovery freezes queued/steered identity, including concurrent renewal", async () => {
  const requests: string[][] = [];
  const events: SeatEvent[] = [];
  const mock = createMockModel({ handler: context => {
    requests.push(context.messages.filter(m => m.role === "user").map(m => typeof m.content === "string" ? m.content : JSON.stringify(m.content)));
    return requests.length === 1 ? { stopReason: "error", errorMessage: refusal } : { content: ["ok"] };
  } });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "failed", cwd: process.cwd(), modelOverride: mock,
    emit: e => { events.push(e); if (e.type === "auth_retry") {
      agent.setToken("concurrent-renewal");
      void agent.prompt("queued");
      agent.steer("steered");
      agent.authRetry({ ...e, token: "concurrent-renewal", expires_at: Date.now() + 60000 });
    } }, callTool: async () => "" });
  await agent.prompt("original");
  expect(requests[1]).toEqual(requests[0]);
  expect(requests.at(-1)?.join(" ")).toContain("queued");
  expect(requests.at(-1)?.join(" ")).toContain("steered");
  expect(events.filter(e => e.type === "absorbed").map(e => e.text)).toEqual(["steered", "queued"]);
});

test("T169 cancellation during recovery never replays the refused prompt", async () => {
  const mock = createMockModel({ handler: () => ({ stopReason: "error", errorMessage: refusal }) });
  let event: any;
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "failed", cwd: process.cwd(), modelOverride: mock,
    emit: e => { if (e.type === "auth_retry") { event = e; agent.abort(); } }, callTool: async () => "" });
  await agent.prompt("original");
  agent.authRetry({ ...event, token: "new", expires_at: Date.now() + 60000 });
  expect(mock.calls.length).toBe(1);
});

test("T169 historical auth and non-auth refusals do not trigger recovery", async () => {
  for (const next of ["ok", "429 rate limit"]) {
    let calls = 0;
    const events: SeatEvent[] = [];
    const mock = createMockModel({ handler: () => ++calls === 1
      ? { stopReason: "error", errorMessage: refusal }
      : next === "ok" ? { content: ["ok"] } : { stopReason: "error", errorMessage: next } });
    const agent = createSeatAgent({ provider: "mock", model: "mock", token: "failed", cwd: process.cwd(), modelOverride: mock,
      emit: e => { events.push(e); if (e.type === "auth_retry") agent.authRetry({ ...e }); }, callTool: async () => "" });
    await agent.prompt("old");
    events.length = 0;
    await agent.prompt("new");
    expect(events.filter(e => e.type === "auth_retry")).toEqual([]);
  }
});

test("T169 a later prompt cannot overtake input queued behind terminal refusal", async () => {
  const requests: string[] = [];
  let reject = true;
  const mock = createMockModel({ handler: context => {
    requests.push(JSON.stringify(context.messages.filter(m => m.role === "user")));
    return reject ? { stopReason: "error", errorMessage: refusal } : { content: ["ok"] };
  } });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "failed", cwd: process.cwd(), modelOverride: mock,
    emit: e => { if (e.type === "auth_retry") { void agent.prompt("queued-first"); agent.authRetry({ ...e }); } }, callTool: async () => "" });
  await agent.prompt("original");
  reject = false;
  await agent.prompt("later");
  expect(requests[1]).toContain("queued-first");
  expect(requests[1]).not.toContain("later");
  expect(requests[2]).toContain("later");
});
