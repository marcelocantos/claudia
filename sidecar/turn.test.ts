import { describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Spool } from "./spool.ts";
import {
  beginTurn,
  classifyCause,
  closeTurn,
  noteDelta,
  noteTool,
  stripStopTokens,
  turnRefusal,
} from "./turn.ts";

describe("classifyCause", () => {
  const cases: [string, string][] = [
    ["hello owner", "owner"],
    ["[claudia] The host restarted at 2026-09-26T01:37:34Z. continue", "restart-nudge"],
    ["[event: sentinel] T219 repair idle:mm2-t65-keys-doors", "sentinel"],
    ["**Impatience incident closed — jevons**", "impatience"],
    ["[Agent mm2-t65-keys-doors responded]\nI'll inspect", "agent-forward"],
    ["[event: rsi-coach] judgment", "rsi"],
    ["[event: capacity] daily budget", "capacity"],
  ];
  for (const [text, cause] of cases) {
    test(cause, () => {
      const got = classifyCause(text);
      expect(got.cause).toBe(cause);
      expect(got.detail.includes("\n")).toBe(false);
    });
  }

  test("detail is one line, not the transcript", () => {
    const got = classifyCause("[event: sentinel] first\nsecond line of the whole thread");
    expect(got.detail).toBe("[event: sentinel] first");
  });
});

describe("stop token", () => {
  test("a whole delta is a field, not visible text", () => {
    const turn = beginTurn({
      seat: "jevons",
      text: "hi",
      meta: { turn_id: "t", session_id: "s", cause: "owner" },
      now: new Date("2026-09-26T02:00:00.000Z"),
    });
    expect(noteDelta(turn, "plan")).toBe("plan");
    expect(noteDelta(turn, "<|eos|>")).toBe("");
    const digest = closeTurn(turn, "end_turn", new Date("2026-09-26T02:00:01.000Z"));
    expect(digest.stop).toBe("stop_token");
    expect(digest.stop_token).toBe("<|eos|>");
    expect(digest.deltas).toBe(1);
    expect(digest.chars).toBe(4);
    expect(digest).not.toHaveProperty("snapshot");
  });

  test("a token split across deltas is not emitted", () => {
    const turn = beginTurn({ seat: "jevons", text: "x", now: new Date("2026-09-26T02:00:00.000Z") });
    expect(noteDelta(turn, "ok<|eo")).toBe("ok");
    expect(noteDelta(turn, "s|>")).toBe("");
    expect(turn.stop_token).toBe("<|eos|>");
    expect(turn.chars).toBe(2);
  });

  test("stripStopTokens on a finished string", () => {
    expect(stripStopTokens("I'll inspect <|eos|>")).toEqual({ visible: "I'll inspect ", token: "<|eos|>" });
    expect(stripStopTokens("<|eos|>").visible).toBe("");
  });
});

describe("digest", () => {
  test("explicit steer cause and tool count survive close", () => {
    const turn = beginTurn({
      seat: "jevons",
      text: "fold this in",
      meta: { cause: "steer", cause_detail: "steer the open turn", turn_id: "st", session_id: "sess" },
      now: new Date("2026-09-26T03:00:00.000Z"),
    });
    noteTool(turn);
    noteDelta(turn, "done");
    const digest = closeTurn(turn, "end_turn", new Date("2026-09-26T03:00:02.000Z"));
    expect(digest.cause).toBe("steer");
    expect(digest.cause_detail).toBe("steer the open turn");
    expect(digest.tool_calls).toBe(1);
    expect(digest.turn_id).toBe("st");
    expect(digest.session_id).toBe("sess");
    expect(digest.stop).toBe("end_turn");
  });

  test("resume=launched is on the digest and the spool line has no snapshot", () => {
    const turn = beginTurn({
      seat: "jevons",
      text: "nudge",
      meta: {
        cause: "restart-nudge",
        cause_detail: "host restarted at 11:37:34",
        resume: "launched",
        turn_id: "n1",
        session_id: "same-session",
      },
      now: new Date("2026-09-26T01:37:34.000Z"),
    });
    const digest = closeTurn(turn, "end_turn", new Date("2026-09-26T01:37:40.000Z"));
    const dir = mkdtempSync(join(tmpdir(), "t870-"));
    const spool = new Spool(dir, new Date("2026-09-26T01:37:40.000Z"));
    const path = spool.append({ ...digest, seat: "jevons" }, new Date("2026-09-26T01:37:40.000Z"));
    const line = readFileSync(path, "utf8").trim();
    const rec = JSON.parse(line);
    expect(rec.type).toBe("turn");
    expect(rec.cause).toBe("restart-nudge");
    expect(rec.resume).toBe("launched");
    expect(rec.session_id).toBe("same-session");
    expect(rec.tool_calls).toBe(0);
    expect(line.includes("snapshot")).toBe(false);
    const typeAt = line.indexOf('"type"');
    expect(typeAt).toBeGreaterThan(0);
    expect(typeAt).toBeLessThan(80);
  });
});

describe("turnRefusal", () => {
  test("names the provider's reason for a refused turn", () => {
    const state = {
      messages: [
        { role: "user", content: "summarize" },
        { role: "assistant", content: [{ type: "text", text: "" }], stopReason: "error",
          errorMessage: "429 usage_limit_reached: weekly limit resets 2026-10-03T23:20Z" },
      ],
    };
    expect(turnRefusal(state)).toBe("429 usage_limit_reached: weekly limit resets 2026-10-03T23:20Z");
  });
  test("is empty for a turn that answered, even with no text", () => {
    expect(turnRefusal({ messages: [{ role: "assistant", content: [], stopReason: "stop" }] })).toBe("");
    expect(turnRefusal({ messages: [] })).toBe("");
    expect(turnRefusal(undefined)).toBe("");
  });
  test("is empty for an aborted turn", () => {
    expect(turnRefusal({ messages: [{ role: "assistant", stopReason: "aborted", errorMessage: "aborted" }] })).toBe("");
  });
  test("still reports a refusal with no reason", () => {
    expect(turnRefusal({ messages: [{ role: "assistant", stopReason: "error" }] })).not.toBe("");
  });
});
