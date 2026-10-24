// 🎯T221 / jevons 🎯T1218: shared native + setTools tool visibility helpers.

import type { SeatEvent } from "./seat.ts";

export type SidecarToolTerminalStatus =
  | "completed"
  | "failed"
  | "cancelled"
  | "denied"
  | "unknown";

type ToolExecutionEnd = {
  toolCallId?: string;
  toolName?: string;
  isError?: boolean;
  result?: {
    content?: Array<{ type?: string; text?: string }>;
    details?: Record<string, unknown>;
    isError?: boolean;
  };
};

export function toolResultPreview(result: ToolExecutionEnd["result"]): string {
  const part = result?.content?.[0];
  if (part?.type === "text" && part.text) return part.text.slice(0, 500);
  return "";
}

/** Map pi-agent-core tool_execution_end facts to a sidecar terminal status. */
export function terminalStatusFromExecutionEnd(event: ToolExecutionEnd): SidecarToolTerminalStatus {
  const details = event.result?.details;
  const text = toolResultPreview(event.result);
  const interrupted =
    details?.__interrupted === true ||
    details?.__synthetic === true ||
    (typeof details?.source === "string" && String(details.source).includes("interrupt"));
  if (interrupted || text.startsWith("Skipped due to")) return "cancelled";
  const blocked =
    text.includes("Tool execution was blocked") ||
    details?.blocked === true ||
    details?.status === "blocked";
  if (blocked) return "denied";
  const isError = event.isError === true || event.result?.isError === true;
  if (isError) return "failed";
  if (event.isError === false || event.result?.isError === false) return "completed";
  return "unknown";
}

export function safeEmitSeatEvent(emit: (ev: SeatEvent) => void, ev: SeatEvent): void {
  try {
    emit(ev);
  } catch {
    // Observability must not change tool execution or results.
  }
}

export function encodeSidecarWireLine(ev: SeatEvent): string {
  return JSON.stringify(ev) + "\n";
}
