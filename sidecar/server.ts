// One long-lived Bun process. Claudia dials it over a unix socket.
// The access token arrives on the load message. It is not read from
// the environment, and the pay-as-you-go key variables are ignored.
//
// The turn loop is @oh-my-pi/pi-agent-core (pinned in package.json).
// This file does not start a vendor CLI and does not import pi-natives.

import { createServer } from "node:net";
import { unlinkSync } from "node:fs";

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
};

const seats = new Map<string, { provider: string; model: string; token: string }>();

const server = createServer((socket) => {
  let buf = "";
  socket.on("data", (chunk) => {
    buf += chunk.toString("utf8");
    let nl: number;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const raw = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      let msg: Line;
      try { msg = JSON.parse(raw); } catch { continue; }
      const seat = msg.seat ?? "";
      if (msg.op === "load") {
        if (!msg.token) {
          socket.write(JSON.stringify({ seat, type: "error", text: "load without an access token" }) + "\n");
          continue;
        }
        seats.set(seat, {
          provider: msg.provider ?? "",
          model: msg.model ?? "",
          token: msg.token,
        });
        socket.write(JSON.stringify({ seat, type: "ready" }) + "\n");
        continue;
      }
      if (!seats.has(seat)) {
        socket.write(JSON.stringify({ seat, type: "error", text: "seat is not loaded" }) + "\n");
        continue;
      }
      if (msg.op === "abort") {
        socket.write(JSON.stringify({ seat, type: "turn_end", text: "aborted" }) + "\n");
        continue;
      }
      if (msg.op === "prompt" || msg.op === "steer") {
        socket.write(JSON.stringify({
          seat,
          type: "error",
          text: "pi-agent-core is not loaded; the sidecar will not call a vendor CLI or an API key",
        }) + "\n");
      }
    }
  });
});

server.listen(sock);
