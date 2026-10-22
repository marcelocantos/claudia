// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { expect, test } from "bun:test";
import type { AssistantMessage } from "@oh-my-pi/pi-ai";
import { savedTranscriptEndsMidTurn } from "./transcript-end.ts";

const usage = {
  input: 1,
  output: 1,
  cacheRead: 0,
  cacheWrite: 0,
  totalTokens: 2,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};

test("completed assistant turn is not mid-turn", () => {
  const assistant: AssistantMessage = {
    role: "assistant",
    content: [{ type: "text", text: "done" }],
    api: "mock",
    provider: "mock",
    model: "mock",
    stopReason: "stop",
    timestamp: 1,
    usage,
  };
  expect(savedTranscriptEndsMidTurn([{ role: "user", content: "hi", timestamp: 1 }, assistant], false)).toBe(false);
});

test("unpaired tool call tail is mid-turn", () => {
  const assistant: AssistantMessage = {
    role: "assistant",
    content: [{ type: "toolCall", id: "c1", name: "Read", arguments: {} }],
    api: "mock",
    provider: "mock",
    model: "mock",
    stopReason: "toolUse",
    timestamp: 2,
    usage,
  };
  expect(savedTranscriptEndsMidTurn([{ role: "user", content: "go", timestamp: 1 }, assistant], false)).toBe(true);
});

test("paired interrupted batch stays mid-turn", () => {
  expect(savedTranscriptEndsMidTurn([], true)).toBe(true);
});
