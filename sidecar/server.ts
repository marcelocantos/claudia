// One long-lived Bun process. Claudia dials it over a unix socket.
// The access token arrives on a load or token message. It is not read from
// the environment, and the pay-as-you-go key variables are ignored.
//
// A token belongs to a plan, not a seat (🎯T159): every seat on a plan uses
// the plan's current token, so one "token" message from the host moves them
// all. A per-seat copy went stale whenever the plan refreshed, and each
// stale seat found out only by being refused.
//
// The turn loop is @oh-my-pi/pi-agent-core (pinned in package.json).

import { createServer } from "node:net";
import { unlinkSync } from "node:fs";
import { dirname, join } from "node:path";
import { createSeatAgent, type HostTool, type SeatAgent } from "./seat.ts";
import { defaultWriter } from "./spool.ts";
import { claimSocket } from "./singleton.ts";
import { PlanTokens } from "./plantokens.ts";
import { HOST_CONNECTION_LOST, HostCalls } from "./hostcalls.ts";
import { SeatStore } from "./store.ts";
import type { SandboxPolicy } from "./coding.ts";

const banned = [
  "ANTHROPIC_API_KEY",
  "OPENAI_API_KEY",
  "XAI_API_KEY",
  "CURSOR_ACCESS_TOKEN",
];
for (const name of banned) delete process.env[name];

const sock = process.argv[2];
if (!sock) {
  console.error("usage: bun server.ts <socket-path>");
  process.exit(2);
}
// 🎯T145: another live sidecar owns this socket; leave it alone.
if (!claimSocket(sock)) {
  console.error(`sidecar: ${sock} is served by another sidecar; exiting`);
  process.exit(0);
}
// Holding the lock, a socket file left here is stale.
try { unlinkSync(sock); } catch { /* absent */ }

// 🎯T166: started with a lifeline, the sidecar lives only as long as the
// process that started it. Only that process holds the pipe's write end;
// when it exits, however it exits, stdin reaches EOF and the sidecar goes
// too, instead of lingering as an orphan a later broker would adopt.
// Seat conversations are written synchronously, so nothing is lost.
if (process.argv.includes("--lifeline=stdin")) {
  const gone = () => {
    console.error("sidecar: the process that started it is gone; exiting");
    process.exit(0);
  };
  process.stdin.on("end", gone);
  process.stdin.on("close", gone);
  process.stdin.on("error", gone);
  process.stdin.on("data", () => {});
  process.stdin.resume();
}

// storeRoot holds each seat's conversation beside the socket (🎯T151), so a
// sidecar restart resumes a seat the host reloads with the same session.
const storeRoot = join(dirname(sock), "omp-seats");

type Line = {
  op?: string;
  seat?: string;
  provider?: string;
  model?: string;
  token?: string;
  request_id?: string;
  expires_at?: number;
  cwd?: string;
  sandbox?: SandboxPolicy;
  text?: string;
  call_id?: string;
  result?: string;
  turn_id?: string;
  session_id?: string;
  cause?: string;
  cause_detail?: string;
  resume?: string;
  tools?: HostTool[];
  preserve?: string;
  pins?: string[];
};

// planTokens is each plan's current access token, keyed by provider.
const planTokens = new PlanTokens();

// setPlanToken makes token the plan's, for every seat on it.
function setPlanToken(provider: string, token: string) {
  planTokens.set(provider, token, seats.values());
}

type Seat = {
  provider: string;
  model: string;
  token: string;
  agent: SeatAgent;
  // calls is the HostCalls of the connection this seat is bound to, so a
  // rebind can fail this seat's in-flight tools without touching others
  // on the old connection (🎯T149).
  calls: HostCalls;
};

const seats = new Map<string, Seat>();

process.on("uncaughtException", (err: NodeJS.ErrnoException) => {
  if (err.code === "EPIPE" || err.code === "ECONNRESET") return;
  console.error("uncaught", err);
});

const server = createServer((socket) => {
  let buf = "";
  const calls = new HostCalls();
  socket.on("error", () => {});
  // A tool call this connection was answering can no longer be answered.
  // Fail it so the turn ends instead of waiting forever (jevons 🎯T927).
  socket.on("close", () => {
    const n = calls.failAll(HOST_CONNECTION_LOST);
    if (n > 0) console.error(`sidecar: host connection closed with ${n} tool call(s) unanswered; failed them`);
  });

  const write = (ev: Record<string, unknown>) => {
    const seat = typeof ev.seat === "string" ? ev.seat : "";
    if (seat && ev.type !== "dropped") {
      try {
        const loaded = seats.get(seat);
        const str = (k: string) => (typeof ev[k] === "string" ? ev[k] as string : undefined);
        const num = (k: string) => (typeof ev[k] === "number" ? ev[k] as number : undefined);
        defaultWriter().append({
          ts: str("ts") ?? new Date().toISOString(),
          seat,
          type: String(ev.type ?? ""),
          text: str("text"),
          call_id: str("call_id"),
          name: str("name"),
          provider: loaded?.provider,
          model: loaded?.model,
          snapshot: ev.snapshot,
          turn_id: str("turn_id"),
          session_id: str("session_id"),
          cause: str("cause"),
          cause_detail: str("cause_detail"),
          started_at: str("started_at"),
          ended_at: str("ended_at"),
          stop: str("stop"),
          tool_calls: num("tool_calls"),
          deltas: num("deltas"),
          chars: num("chars"),
          stop_token: str("stop_token"),
          resume: str("resume"),
        });
      } catch (err) {
        console.error("spool append failed", err);
      }
    }
    try {
      socket.write(JSON.stringify(ev) + "\n");
    } catch {
      /* client gone; do not take down the sidecar */
    }
  };

  socket.on("data", (chunk) => {
    buf += chunk.toString("utf8");
    let nl: number;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const raw = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      let msg: Line;
      try { msg = JSON.parse(raw); } catch { continue; }
      const seat = msg.seat ?? "";
      const callTool = (callId: string, name: string, args: string) => {
        const answer = calls.open(callId, seat);
        write({ type: "tool_call", call_id: callId, name, text: args });
        return answer;
      };
      void handle(msg, seat, write, callTool, calls).catch((err: unknown) => {
        write({ seat, type: "error", text: err instanceof Error ? err.message : String(err) });
      });
    }
  });
});

async function handle(
  msg: Line,
  seat: string,
  write: (ev: Record<string, unknown>) => void,
  callTool: (callId: string, name: string, args: string) => Promise<string>,
  calls: HostCalls,
): Promise<void> {
  if (msg.op === "tool_result") {
    calls.settle(msg.call_id ?? "", msg.result ?? "");
    return;
  }
  if (msg.op === "token") {
    // 🎯T159: the plan's token changed; every seat on it takes it.
    setPlanToken(msg.provider ?? "", msg.token ?? "");
    return;
  }
  if (msg.op === "adopt") {
    // A seat that is not already running is not created (🎯T869). ResumeAll
    // treats that as a miss and launches, and only the launch is nudged.
    if (!msg.token) {
      write({ seat, type: "error", text: "adopt without an access token" });
      return;
    }
    const existing = seats.get(seat);
    if (!existing) {
      write({ seat, type: "error", text: "seat is not loaded" });
      return;
    }
    if (existing.provider !== msg.provider) {
      write({ seat, type: "error", reason: "seat_identity_mismatch",
        text: `loaded seat is ${existing.provider}; registry asked for ${msg.provider ?? ""}` });
      return;
    }
    if (!(await switchIfAsked(existing, msg, seat, write))) return;
    rebindSeat(existing, msg, seat, write, callTool, calls);
    write({ seat, type: "ready", how: "adopted", bound_tools: existing.agent.boundToolNames(), sandbox: existing.agent.sandboxMode() });
    return;
  }
  if (msg.op === "load") {
    if (!msg.token) {
      write({ seat, type: "error", text: "load without an access token" });
      return;
    }
    const existing = seats.get(seat);
    // A load of a loaded seat keeps its conversation. A different provider
    // or model is a switch in place (🎯T195): the agent takes the new model
    // between turns, and nothing is summarised or handed over.
    if (existing) {
      const switched = msg.provider !== existing.provider || (!!msg.model && msg.model !== existing.model);
      if (!(await switchIfAsked(existing, msg, seat, write))) return;
      rebindSeat(existing, msg, seat, write, callTool, calls);
      write({
        seat,
        type: "ready",
        how: switched ? "switched" : "adopted",
        request_id: msg.request_id,
        provider: existing.provider,
        model: existing.model,
        bound_tools: existing.agent.boundToolNames(),
        sandbox: existing.agent.sandboxMode(),
      });
      return;
    }
    // A host that names the seat's session gets a durable conversation; one
    // that does not (an older host) gets the old in-memory seat.
    const store = msg.session_id ? new SeatStore(storeRoot, seat, msg.session_id) : undefined;
    // A seat joining a plan runs on the plan's newest token.
    setPlanToken(msg.provider ?? "", msg.token);
    const agent = createSeatAgent({
      provider: msg.provider ?? "",
      model: msg.model ?? "",
      token: planTokens.get(msg.provider ?? "") ?? msg.token,
      cwd: msg.cwd ?? "",
      emit: (ev) => write({ seat, ...ev }),
      callTool,
      tools: msg.tools,
      store,
      preserve: msg.preserve,
      pins: msg.pins,
      sandbox: msg.sandbox,
    });
    seats.set(seat, {
      provider: msg.provider ?? "",
      model: msg.model ?? "",
      token: planTokens.get(msg.provider ?? "") ?? msg.token,
      agent,
      calls,
    });
    write({ seat, type: "ready", how: "launched", restored: agent.restored, bound_tools: agent.boundToolNames(), sandbox: agent.sandboxMode() });
    return;
  }
  const loaded = seats.get(seat);
  if (!loaded) {
    write({ seat, type: "error", text: "seat is not loaded" });
    return;
  }
  if (msg.op === "auth_retry") {
    loaded.agent.authRetry(msg);
    return;
  }
  if (msg.op === "abort") {
    loaded.agent.abort();
    write({ seat, type: "turn_end", text: "aborted", snapshot: loaded.agent.snapshot() });
    return;
  }
  if (msg.op === "drop") {
    loaded.agent.abort();
    seats.delete(seat);
    write({ seat, type: "dropped" });
    return;
  }
  if (msg.op === "steer") {
    loaded.agent.steer(msg.text ?? "", promptMeta(msg));
    return;
  }
  if (msg.op === "prompt") {
    await loaded.agent.prompt(msg.text ?? "", promptMeta(msg));
  }
}

function rebindSeat(
  existing: Seat,
  msg: Line,
  seat: string,
  write: (ev: Record<string, unknown>) => void,
  callTool: (callId: string, name: string, args: string) => Promise<string>,
  calls: HostCalls,
) {
  // A rebind onto a different connection: nothing on the old one will
  // answer this seat's in-flight tools (🎯T149). Same-connection load/adopt
  // keeps them — the host is still there and can still tool_result.
  if (existing.calls !== calls) {
    const n = existing.calls.failSeat(seat, HOST_CONNECTION_LOST);
    if (n > 0) console.error(`sidecar: rebound ${seat} with ${n} unanswered tool call(s); failed them`);
    existing.calls = calls;
  }
  if (msg.provider) existing.provider = msg.provider;
  if (msg.token) setPlanToken(existing.provider, msg.token);
  existing.agent.rebind((ev) => write({ seat, ...ev }), callTool);
  if (msg.tools) existing.agent.setHostTools(msg.tools);
  if (msg.cwd) existing.agent.setCwd(msg.cwd);
  // A load names the seat's sandbox every time; absent clears it (🎯T178).
  if (msg.op === "load" || msg.sandbox !== undefined) existing.agent.setSandbox(msg.sandbox);
  // The host sends its current compaction steer on every load (🎯T152).
  existing.agent.setContext(msg.preserve, msg.pins);
}

// switchIfAsked moves a loaded seat to the provider and model msg names, when
// they differ, before anything is rebound: a conversation that must be folded
// to fit is folded by the model that wrote it, on its own plan's token
// (🎯T195). It reports false, having written the refusal, when the seat
// cannot switch; the seat then stays as it was.
async function switchIfAsked(
  existing: Seat,
  msg: Line,
  seat: string,
  write: (ev: Record<string, unknown>) => void,
): Promise<boolean> {
  const provider = msg.provider || existing.provider;
  const model = msg.model || existing.model;
  if (provider === existing.provider && model === existing.model) return true;
  try {
    await existing.agent.switchModel(provider, model);
  } catch (err) {
    write({
      seat,
      type: "error",
      reason: "switch_refused",
      request_id: msg.request_id,
      text: err instanceof Error ? err.message : String(err),
    });
    return false;
  }
  existing.model = model;
  return true;
}

function promptMeta(msg: Line) {
  return {
    turn_id: msg.turn_id,
    session_id: msg.session_id,
    cause: msg.cause,
    cause_detail: msg.cause_detail,
    resume: msg.resume,
  };
}

server.listen(sock);
