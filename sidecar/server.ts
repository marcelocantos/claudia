// One long-lived Bun process. Claudia dials it over a unix socket.
// The access token arrives on the load message. It is not read from
// the environment, and the pay-as-you-go key variables are ignored.
//
// The turn loop is @oh-my-pi/pi-agent-core (pinned in package.json).

import { createServer } from "node:net";
import { unlinkSync } from "node:fs";
import { createSeatAgent, type SeatAgent } from "./seat.ts";

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
try { unlinkSync(sock); } catch { /* absent */ }

type Line = {
  op?: string;
  seat?: string;
  provider?: string;
  model?: string;
  token?: string;
  text?: string;
  call_id?: string;
  result?: string;
};

type Seat = {
  provider: string;
  model: string;
  token: string;
  agent: SeatAgent;
};

const seats = new Map<string, Seat>();

const server = createServer((socket) => {
  let buf = "";
  const pending = new Map<string, (result: string) => void>();

  const write = (ev: Record<string, unknown>) => {
    socket.write(JSON.stringify(ev) + "\n");
  };

  const callTool = (callId: string, name: string, args: string) => {
    write({ type: "tool_call", call_id: callId, name, text: args });
    return new Promise<string>((resolve) => {
      pending.set(callId, resolve);
    });
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
      void handle(msg, seat, write, callTool, pending).catch((err: unknown) => {
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
  pending: Map<string, (result: string) => void>,
): Promise<void> {
  if (msg.op === "tool_result") {
    const done = pending.get(msg.call_id ?? "");
    if (done) {
      pending.delete(msg.call_id ?? "");
      done(msg.result ?? "");
    }
    return;
  }
  if (msg.op === "load") {
    if (!msg.token) {
      write({ seat, type: "error", text: "load without an access token" });
      return;
    }
    const existing = seats.get(seat);
    if (existing) {
      existing.token = msg.token;
      existing.agent.setToken(msg.token);
      if (msg.model && msg.model !== existing.model) {
        existing.agent.setModel(msg.provider ?? existing.provider, msg.model);
        existing.model = msg.model;
      }
      write({ seat, type: "ready" });
      return;
    }
    const agent = createSeatAgent({
      provider: msg.provider ?? "",
      model: msg.model ?? "",
      token: msg.token,
      emit: (ev) => write({ seat, ...ev }),
      callTool,
    });
    seats.set(seat, {
      provider: msg.provider ?? "",
      model: msg.model ?? "",
      token: msg.token,
      agent,
    });
    write({ seat, type: "ready" });
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
  if (msg.op === "steer") {
    loaded.agent.steer(msg.text ?? "");
    return;
  }
  if (msg.op === "prompt") {
    await loaded.agent.prompt(msg.text ?? "");
  }
}

server.listen(sock);
