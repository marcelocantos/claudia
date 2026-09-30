// 🎯T154: a single oversized tool result is truncated before it enters the
// conversation. The host keeps the full text.

import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Tokenizer } from "@oh-my-pi/pi-agent-core";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context } from "@oh-my-pi/pi-ai";
import { codingTools } from "./coding.ts";
import { createSeatAgent, type HostTool, type SeatEvent } from "./seat.ts";
import {
  TOOL_RESULT_ABS_CAP,
  TOOL_RESULT_TRUNCATED,
  TOOL_RESULT_WINDOW_SHARE,
  capToolText,
  toolResultBound,
  truncateToolResult,
} from "./truncate.ts";

registerMockApi();

describe("truncateToolResult (🎯T154)", () => {
  test("a result under the bound is unchanged", () => {
    const text = "hello, this is a short tool result";
    const got = truncateToolResult(text, 1000, "Read");
    expect(got).toEqual({ text, truncated: false, original: text.length, dropped: 0 });
  });

  test("an oversized result keeps the head and tail, states the cut, and stays at or under the bound", () => {
    const head = "HEAD-UNIQUE-aaa";
    const tail = "TAIL-UNIQUE-zzz";
    const middle = "m".repeat(50_000);
    const text = head + middle + tail;
    const bound = 4_000;
    const got = truncateToolResult(text, bound, "jevons_transcript_read");
    expect(got.truncated).toBe(true);
    expect(got.original).toBe(text.length);
    expect(got.dropped).toBeGreaterThan(40_000);
    expect(got.text.length).toBeLessThanOrEqual(bound);
    expect(got.text.startsWith(head)).toBe(true);
    expect(got.text.endsWith(tail)).toBe(true);
    expect(got.text).toContain(TOOL_RESULT_TRUNCATED);
    expect(got.text).toContain(`${got.dropped} of ${text.length} chars`);
    expect(got.text).toContain("from jevons_transcript_read");
    expect(got.text).toContain("Read a narrower range");
    expect(got.text).toContain("page with its own limit/offset");
    // The middle must not survive whole.
    expect(got.text.includes(middle)).toBe(false);
  });

  test("capToolText does not mutate the original string", () => {
    const original = "HEAD" + "x".repeat(10_000) + "TAIL";
    const copy = original;
    const out = capToolText(original, 500, "Bash");
    expect(original).toBe(copy);
    expect(out.length).toBeLessThanOrEqual(500);
    expect(out).not.toBe(original);
  });

  test("the bound is a fraction of the window, capped absolutely", () => {
    const small = toolResultBound(8_000);
    const mid = toolResultBound(100_000);
    const huge = toolResultBound(1_000_000);
    expect(small).toBe(Math.max(2048, Math.floor(8_000 * TOOL_RESULT_WINDOW_SHARE * 4)));
    expect(mid).toBe(Math.floor(100_000 * TOOL_RESULT_WINDOW_SHARE * 4));
    expect(huge).toBe(TOOL_RESULT_ABS_CAP);
    expect(toolResultBound(0)).toBe(TOOL_RESULT_ABS_CAP);
  });
});

describe("coding tools cap their conversation copy (🎯T154)", () => {
  test("Read of a large file keeps head and tail and names a narrower read", async () => {
    const dir = mkdtempSync(join(tmpdir(), "t154-"));
    try {
      const head = "FILE-HEAD-aaa";
      const tail = "FILE-TAIL-zzz";
      writeFileSync(join(dir, "big.txt"), head + "Q".repeat(80_000) + tail);
      const tools = codingTools(() => dir, () => 3_000);
      const read = tools.find((t) => t.name === "Read");
      if (!read) throw new Error("no Read tool");
      const result = (await read.execute("c1", { path: "big.txt" })) as {
        content: { type: string; text: string }[];
      };
      const text = result.content[0].text;
      expect(text.length).toBeLessThanOrEqual(3_000);
      expect(text.startsWith(head)).toBe(true);
      expect(text.endsWith(tail)).toBe(true);
      expect(text).toContain(TOOL_RESULT_TRUNCATED);
      expect(text).toContain("from Read");
      expect(text).toContain("Read a narrower range");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("a several-hundred-kilobyte host tool result (🎯T154)", () => {
  const window = 100_000;
  const tokenizer = new Tokenizer(null);

  function sent(context: Context): number {
    let n = 0;
    for (const m of context.messages) n += tokenizer.countMessage(m as never);
    const prefix = [...(context.systemPrompt ?? [])];
    for (const t of context.tools ?? []) prefix.push(t.name, t.description ?? "", JSON.stringify(t.parameters ?? {}));
    return n + tokenizer.countTokens(prefix);
  }

  test("leaves the next request under the bound, with the marker present, and does not drop the host's copy", async () => {
    const head = "HOST-HEAD-aaa";
    const tail = "HOST-TAIL-zzz";
    const payload = head + "H".repeat(400_000) + tail; // 400 KB-class result
    let hostHeld = "";
    const bound = toolResultBound(window);
    const requests: { chars: number; tokens: number; body: string }[] = [];
    let call = 0;
    const mock = createMockModel({
      contextWindow: window,
      handler: (context: Context): MockResponse => {
        const body = JSON.stringify(context.messages);
        requests.push({ chars: body.length, tokens: sent(context), body });
        call++;
        if (call === 1) {
          return { content: [{ type: "toolCall", name: "jevons_transcript_read", arguments: { limit: 40 } }], usage: { input: 100, output: 1 } };
        }
        return { content: ["done"], usage: { input: sent(context), output: 1 } };
      },
    });
    const events: SeatEvent[] = [];
    const tools: HostTool[] = [{ name: "jevons_transcript_read", description: "read a transcript", input_schema: { type: "object" } }];
    const agent = createSeatAgent({
      provider: "mock",
      model: "mock-model",
      token: "t",
      cwd: process.cwd(),
      emit: (ev) => events.push(ev),
      callTool: async () => {
        hostHeld = payload;
        return payload;
      },
      tools,
      modelOverride: mock as never,
    });

    await agent.prompt("read the transcript");

    expect(hostHeld.length).toBe(payload.length); // host still holds the full text
    expect(hostHeld).toBe(payload);
    const follow = requests[1];
    expect(follow).toBeDefined();
    expect(follow.body).toContain(TOOL_RESULT_TRUNCATED);
    expect(follow.body).toContain(head);
    expect(follow.body).toContain(tail);
    expect(follow.body).toContain("from jevons_transcript_read");
    expect(follow.body).toContain("Read a narrower range");
    expect(follow.body).toContain("page with its own limit/offset");
    // The 400k middle must not enter the next request.
    expect(follow.body.includes("H".repeat(100_000))).toBe(false);
    // The tool result itself is under the per-seat bound. The whole request
    // (prompt, tool call, truncated result) is far under several hundred KB.
    expect(follow.chars).toBeLessThan(bound + 20_000);
    expect(follow.chars).toBeLessThan(100_000);
    const end = events.filter((e) => e.type === "turn_end").at(-1);
    expect(end?.error).toBeUndefined();
  });
});
