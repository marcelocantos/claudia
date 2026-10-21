import { describe, expect, test } from "bun:test";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";

import { matchesProtectedPattern, type ProtectedPathRule } from "./protected.ts";
import { recordReadView, refusalForTruncatedWrite, clearReadView } from "./readguard.ts";
import { writeFile, type SandboxPolicy } from "./execution.ts";

const ledgerRule: ProtectedPathRule = {
  pattern: "**/bullseye.yaml",
  reason:
    "refusing mutation of bullseye.yaml — agents must not edit that file. File, status, achieve, and query go through jevons_target_file or bullseye MCP/CLI (`bullseye commit` / `bullseye query`). 🎯T546",
};

describe("protected paths (🎯T209)", () => {
  test("**/bullseye.yaml matches worktree paths", () => {
    expect(matchesProtectedPattern("bullseye.yaml", "**/bullseye.yaml")).toBe(true);
    expect(matchesProtectedPattern("wt/bullseye.yaml", "**/bullseye.yaml")).toBe(true);
    expect(matchesProtectedPattern("other.yaml", "**/bullseye.yaml")).toBe(false);
  });

  test("write to bullseye.yaml is refused with the host reason", () => {
    const dir = mkdtempSync(join(tmpdir(), "t209-prot-"));
    try {
      writeFileSync(join(dir, "bullseye.yaml"), "targets:\n", "utf8");
      const policy: SandboxPolicy = { protected_paths: [ledgerRule] };
      expect(() => writeFile(dir, "bullseye.yaml", "x", policy)).toThrow(/T546/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("truncated read guard (🎯T209)", () => {
  test("refuses a whole-file write sized like a truncated read", () => {
    const abs = "/tmp/wt/big.yaml";
    const full = "x".repeat(200_000);
    const seen = full.slice(0, 99_997);
    recordReadView(abs, full, seen);
    const msg = refusalForTruncatedWrite(abs, "big.yaml", Buffer.byteLength(seen, "utf8"));
    expect(msg).toMatch(/truncated/i);
    expect(msg).toMatch(/99.?997/);
    expect(msg).toMatch(/200.?000/);
    clearReadView(abs);
  });
});
