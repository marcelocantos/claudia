// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { expect, test } from "bun:test";
import { mkdtemp, mkdir, readFile, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

test("production auth helper never signs in under no-login, including force-login environment", async () => {
  const root = await mkdtemp(join(tmpdir(), "claudia-auth-policy-"));
  try {
    // Exercise the actual executable source. Only the vendor dependency is
    // replaced; either vendor method reports which branch was reached.
    await writeFile(join(root, "auth.ts"), await readFile(join(import.meta.dir, "auth.ts")));
    const module = join(root, "node_modules", "@oh-my-pi", "pi-ai");
    await mkdir(module, { recursive: true });
    await writeFile(join(module, "package.json"), JSON.stringify({ type: "module", main: "index.js" }));
    await writeFile(join(module, "index.js"), `
export function getProviderDefinition() { return {login: async () => { throw new Error("INTERACTIVE LOGIN REACHED"); }}; }
export async function refreshOAuthToken(provider, existing) {
  if (provider !== "anthropic" || existing.refresh !== "cached-refresh") throw new Error("wrong refresh contract");
  return {access: "renewed", refresh: "next", expires: Date.now() + 3600000};
}
`);
    for (const [verb, refresh] of [["refresh", "cached-refresh"], ["refresh", ""], ["login", "cached-refresh"]]) {
      const proc = Bun.spawn([process.execPath, join(root, "auth.ts"), verb, "anthropic", "--no-login"], {
        env: { PATH: process.env.PATH, HOME: root, OMP_FORCE_LOGIN: "anthropic" },
        stdin: new Blob([JSON.stringify({ access_token: "expired", refresh_token: refresh, expiry: "2000-01-01T00:00:00Z" })]),
        stdout: "pipe", stderr: "pipe",
      });
      const [stdout, stderr, exit] = await Promise.all([
        new Response(proc.stdout).text(), new Response(proc.stderr).text(), proc.exited,
      ]);
      expect(stderr).not.toContain("INTERACTIVE LOGIN REACHED");
      if (verb === "refresh" && refresh !== "") {
        expect(exit).toBe(0);
        expect(JSON.parse(stdout).access_token).toBe("renewed");
      } else {
        expect(exit).toBe(1);
        expect(stderr).toContain("sign-in refused");
      }
    }
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
