// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { createServer } from "node:net";

const [sock, mode] = process.argv.slice(2);
if (mode === "exit") {
  console.error("controlled startup failure");
  process.exit(17);
}
if (mode === "held") {
  console.error("setup-held");
  await new Promise<void>(resolve => process.stdin.once("data", () => resolve()));
}
const server = createServer(socket => {
  socket.on("data", () => socket.destroy());
});
await new Promise<void>((resolve, reject) => {
  server.once("error", reject);
  server.listen(sock, resolve);
});
console.log("abort-fixture-ready");
