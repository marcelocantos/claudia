// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { spawnSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Fail early as an environment outage, rather than spending time on socket
// timeouts or silently omitting macOS sandbox coverage. No provider is called.
const dir = mkdtempSync(join(tmpdir(), "claudia-gate-preflight-"));
try {
  const server = createServer();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(join(dir, "probe.sock"), () => server.close((err) => err ? reject(err) : resolve()));
  });
  if (process.platform === "darwin") {
    const result = spawnSync("/usr/bin/sandbox-exec", ["-p", "(version 1)(allow default)", "/usr/bin/true"]);
    if (result.error || result.status !== 0) {
      throw new Error(`macOS sandbox unavailable: ${result.error ?? result.stderr.toString().trim()}`);
    }
  } else {
    console.log("Sidecar gate: macOS seatbelt coverage requires the macOS gate.");
  }
} catch (err) {
  console.error(`OUTAGE: sidecar test prerequisites unavailable: ${err}`);
  process.exitCode = 2;
} finally {
  rmSync(dir, { recursive: true, force: true });
}
