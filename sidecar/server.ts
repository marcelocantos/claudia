// One long-lived Bun process. Claudia dials it over a unix socket.
// The access token arrives on the load message. It is not read from
// the environment, and the pay-as-you-go key variables are ignored.
//
// The turn loop is @oh-my-pi/pi-agent-core (pinned in package.json).

import { createServer } from "node:net";
import { unlinkSync } from "node:fs";
import { dirname, join } from "node:path";
import { createSeatAgent, type HostTool, type SeatAgent } from "./seat.ts";
import { defaultWriter } from "./spool.ts";
import { claimSocket } from "./singleton.ts";
import { HOST_CONNECTION_LOST, HostCalls } from "./hostcalls.ts";
import { SeatStore } from "./store.ts";

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

// storeRoot holds each seat's conversation beside the socket (🎯T151), so a
// sidecar restart resumes a seat the host reloads with the same session.
const storeRoot = join(dirname(sock), "omp-seats");

type Line = {
  op?: string;
  seat?: string;
  provider?: string;
  model?: string;
  summary_only?: boolean;
  token?: string;
  cwd?: string;
  text?: string;
  call_id?: string;
  result?: string;
  turn_id?: string;
  session_id?: string;
  cause?: string;
  cause_detail?: string;
  resume?: string;
  tools?: HostTool[];
};

type Seat = {
  provider: string;
  model: string;
  token: string;
  summaryOnly: boolean;
  agent: SeatAgent;
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
    // A one-shot transfer seat carries a full predecessor transcript.
    // Return its events to the caller without duplicating them in the
    // durable fleet spool.
    if (seat && ev.type !== "dropped" && !seats.get(seat)?.summaryOnly) {
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

  const callTool = (callId: string, name: string, args: string) => {
    const answer = calls.open(callId);
    write({ type: "tool_call", call_id: callId, name, text: args });
    return answer;
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
    if (existing.provider !== msg.provider || existing.summaryOnly !== (msg.summary_only === true)) {
      write({ seat, type: "error", reason: "seat_identity_mismatch",
        text: `loaded seat is ${existing.provider}; registry asked for ${msg.provider ?? ""}` });
      return;
    }
    rebindSeat(existing, msg, seat, write, callTool);
    write({ seat, type: "ready", how: "adopted" });
    return;
  }
  if (msg.op === "load") {
    if (!msg.token) {
      write({ seat, type: "error", text: "load without an access token" });
      return;
    }
    const existing = seats.get(seat);
    const summaryOnly = msg.summary_only === true;
    // A reconnect keeps the session. A provider switch mints a fresh
    // work agent: handing the old in-memory transcript to the new provider
    // would bypass the bounded transfer brief.
    if (existing && existing.provider === msg.provider && existing.summaryOnly === summaryOnly) {
      rebindSeat(existing, msg, seat, write, callTool);
      write({ seat, type: "ready", how: "adopted" });
      return;
    }
    // A host that names the seat's session gets a durable conversation; one
    // that does not (an older host) gets the old in-memory seat.
    const store = !summaryOnly && msg.session_id ? new SeatStore(storeRoot, seat, msg.session_id) : undefined;
    const agent = createSeatAgent({
      provider: msg.provider ?? "",
      model: msg.model ?? "",
      token: msg.token,
      cwd: msg.cwd ?? "",
      summaryOnly,
      emit: (ev) => write({ seat, ...ev }),
      callTool,
      tools: msg.tools,
      store,
    });
    if (existing) existing.agent.abort();
    seats.set(seat, {
      provider: msg.provider ?? "",
      model: msg.model ?? "",
      token: msg.token,
      summaryOnly,
      agent,
    });
    write({ seat, type: "ready", how: "launched" });
    return;
  }
  const loaded = seats.get(seat);
  if (!loaded) {
    write({ seat, type: "error", text: "seat is not loaded" });
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
) {
  if (msg.token) {
    existing.token = msg.token;
    existing.agent.setToken(msg.token);
  }
  existing.agent.rebind((ev) => write({ seat, ...ev }), callTool);
  if (msg.tools) existing.agent.setHostTools(msg.tools);
  if (msg.cwd) existing.agent.setCwd(msg.cwd);
  if (msg.model && msg.model !== existing.model) {
    existing.agent.setModel(msg.provider ?? existing.provider, msg.model);
    existing.model = msg.model;
  }
  if (msg.provider) existing.provider = msg.provider;
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
