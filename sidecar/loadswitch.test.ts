// 🎯T195: only an explicit switch may fail a load of a loaded seat.

import { describe, expect, test } from "bun:test";
import { switchOnLoad } from "./loadswitch.ts";

const busy = async () => {
  throw new Error("switch refused: a turn is running");
};

describe("a load of a loaded seat", () => {
  test("naming the seat's own provider and model is no switch", async () => {
    let called = false;
    const seat = { provider: "xai-oauth", model: "grok-4.7" };
    const out = await switchOnLoad(seat, { provider: "xai-oauth", model: "grok-4.7" }, async () => {
      called = true;
    });
    expect(out).toEqual({ ok: true, switched: false });
    expect(called).toBe(false);
  });

  test("an empty provider or model keeps the seat's own", async () => {
    const seat = { provider: "xai-oauth", model: "grok-4.7" };
    expect(await switchOnLoad(seat, {}, busy)).toEqual({ ok: true, switched: false });
  });

  test("an explicit switch moves the seat", async () => {
    const seat = { provider: "anthropic", model: "claude-opus-5" };
    const asked: string[] = [];
    const out = await switchOnLoad(seat, { provider: "openai-codex", model: "gpt-5.5", request_id: "r1" }, async (p, m) => {
      asked.push(`${p}/${m}`);
    });
    expect(out).toEqual({ ok: true, switched: true });
    expect(asked).toEqual(["openai-codex/gpt-5.5"]);
    expect(seat).toEqual({ provider: "openai-codex", model: "gpt-5.5" });
  });

  test("an explicit switch the seat refuses fails the load and leaves the seat", async () => {
    const seat = { provider: "anthropic", model: "claude-opus-5" };
    const out = await switchOnLoad(seat, { provider: "openai-codex", model: "gpt-5.5", request_id: "r1" }, busy);
    expect(out).toEqual({ ok: false, error: "switch refused: a turn is running" });
    expect(seat).toEqual({ provider: "anthropic", model: "claude-opus-5" });
  });

  test("a re-attach naming another model mid-turn keeps the seat working on its own model", async () => {
    // A re-attach (a broker grant, a reconnect) can name a model other than
    // the one the seat is on while a turn runs. Failing that load would end
    // a working seat over a label; it keeps the seat on its own model.
    const seat = { provider: "xai-oauth", model: "grok-4.7" };
    const out = await switchOnLoad(seat, { provider: "xai-oauth", model: "grok-4.7-build" }, busy);
    expect(out).toEqual({ ok: true, switched: false, refused: "switch refused: a turn is running" });
    expect(seat).toEqual({ provider: "xai-oauth", model: "grok-4.7" });
  });
});
