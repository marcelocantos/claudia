// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

test("OMP canonical journal preserves service facts and refuses unsafe recovery", async () => {
  const root = mkdtempSync(join(tmpdir(), "claudia-omp-journal-"));
  try {
    const child = Bun.spawn([process.execPath, join(import.meta.dir, "testdata/omp-journal-fixture.ts"), root], {
      env: { PATH: process.env.PATH ?? "", HOME: root, TMPDIR: root,
        XDG_CONFIG_HOME: join(root, "config"), XDG_DATA_HOME: join(root, "data"),
        XDG_STATE_HOME: join(root, "state"), XDG_CACHE_HOME: join(root, "cache"),
        PI_CONFIG_DIR: ".omp", PI_CODING_AGENT_DIR: join(root, ".omp/agent"),
        OMP_PROFILE: "", PI_PROFILE: "", PI_NO_TITLE: "1" },
      stdout: "pipe", stderr: "pipe",
    });
    const [out, err, exit] = await Promise.all([
      new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited,
    ]);
    expect({ exit, err }).toEqual({ exit: 0, err: "" });
    expect(out.trim()).toBe("OMP journal fixture passed");
  } finally { rmSync(root, { recursive: true, force: true }); }
});
