// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// 🎯T214: whether a restored seat store ends inside a turn (broker restart nudge).

import { unpairedToolCallTail, type AgentMessage } from "@oh-my-pi/pi-agent-core";

/** Whether saved messages end inside a turn, not at a completed assistant reply. */
export function savedTranscriptEndsMidTurn(
  messages: readonly AgentMessage[],
  pairedInterruptedToolBatch: boolean,
): boolean {
  if (pairedInterruptedToolBatch) return true;
  if (messages.length === 0) return false;
  const last = messages[messages.length - 1];
  if (last.role === "user") return true;
  if (last.role === "toolResult") return true;
  if (unpairedToolCallTail(messages)) return true;
  return false;
}
