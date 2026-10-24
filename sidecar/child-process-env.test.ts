// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { cloneChildProcessEnv, spawnEnvForSeat, validateChildProcessEnv } from "./child-process-env.ts";
import { codingTools } from "./coding.ts";
import { cursorExecHandlers } from "./cursorexec.ts";

describe("child_process_env overlay", () => {
  test("validate rejects malformed keys", () => {
    expect(() => validateChildProcessEnv({ "": "x" })).toThrow();
    expect(() => validateChildProcessEnv({ "BAD=KEY": "x" })).toThrow();
    expect(() => validateChildProcessEnv({ BAD: "\0" })).toThrow();
    validateChildProcessEnv({ CLAUDIA_T218: "ok" });
  });

  test("spawn merge does not mutate process.env", () => {
    const marker = `CLAUDIA_T218_BASE_${Date.now()}`;
    const before = process.env[marker];
    const merged = spawnEnvForSeat({ CLAUDIA_T218: "overlay" });
    expect(merged.CLAUDIA_T218).toBe("overlay");
    expect(process.env.CLAUDIA_T218).toBeUndefined();
    expect(process.env[marker]).toBe(before);
  });

  test("concurrent seats do not cross-leak overlays", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "t218-"));
    const a = codingTools(() => dir, () => 100_000, () => undefined, () => spawnEnvForSeat({ CLAUDIA_T218_SEAT: "alpha" }));
    const b = codingTools(() => dir, () => 100_000, () => undefined, () => spawnEnvForSeat({ CLAUDIA_T218_SEAT: "beta" }));
    const bashA = a.find((t) => t.name === "Bash")!;
    const bashB = b.find((t) => t.name === "Bash")!;
    const outA = await bashA.execute("a", { command: "printf %s \"$CLAUDIA_T218_SEAT\"" });
    const outB = await bashB.execute("b", { command: "printf %s \"$CLAUDIA_T218_SEAT\"" });
    const textA = (outA.content[0] as { text: string }).text;
    const textB = (outB.content[0] as { text: string }).text;
    expect(textA).toBe("alpha");
    expect(textB).toBe("beta");
    const shared = { CLAUDIA_T218_SEAT: "alpha" };
    cloneChildProcessEnv(shared).CLAUDIA_T218_SEAT = "mutated";
    expect(shared.CLAUDIA_T218_SEAT).toBe("alpha");
    fs.rmSync(dir, { recursive: true, force: true });
  });

  test("explicit PATH in overlay wins over login-shell setup", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "t218-path-"));
    const bin = path.join(dir, "bin");
    fs.mkdirSync(bin);
    fs.writeFileSync(path.join(bin, "t218-echo"), "#!/bin/sh\necho PATH_WIN\n", "utf8");
    fs.chmodSync(path.join(bin, "t218-echo"), 0o755);
    const cursor = cursorExecHandlers(
      () => dir,
      () => 100_000,
      () => undefined,
      () => spawnEnvForSeat({ PATH: bin }),
    ) as any;
    const r = await cursor.shell({ command: "t218-echo" });
    expect(r.content[0].text.trim()).toBe("PATH_WIN");
    fs.rmSync(dir, { recursive: true, force: true });
  });
});
