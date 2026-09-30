import { expect, test } from "bun:test";
import { PlanTokens, type PlanSeat } from "./plantokens.ts";

// 🎯T159: one token per plan. A plan's token reaches every seat on it, and
// no seat on another plan.
test("a plan token reaches every seat on the plan and no other", () => {
  const got: Record<string, string[]> = {};
  const seat = (name: string, provider: string, token: string): PlanSeat => ({
    provider,
    token,
    agent: { setToken: (t) => (got[name] = [...(got[name] ?? []), t]) },
  });
  const seats = [seat("po", "anthropic", "old"), seat("worker", "anthropic", "old"), seat("grok", "xai-oauth", "x")];
  const plans = new PlanTokens();

  expect(plans.set("anthropic", "fresh", seats)).toBe(2);
  expect(plans.get("anthropic")).toBe("fresh");
  expect(got).toEqual({ po: ["fresh"], worker: ["fresh"] });
  expect(seats.map((s) => s.token)).toEqual(["fresh", "fresh", "x"]);

  // The same token again moves nothing.
  expect(plans.set("anthropic", "fresh", seats)).toBe(0);
  // An empty provider or token is ignored.
  expect(plans.set("", "t", seats)).toBe(0);
  expect(plans.set("anthropic", "", seats)).toBe(0);
  expect(plans.get("anthropic")).toBe("fresh");
});
