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

/** pi-agent-core {@link SyntheticToolResultDetails.source} values we classify. */
const SYNTHETIC_TOOL_RESULT_SOURCES = new Set([
  "assistant_stop_aborted",
  "assistant_stop_error",
  "assistant_stop_skipped",
  "assistant_stop_length",
  "interrupt_skipped",
]);

export function toolResultPreview(result: ToolExecutionEnd["result"]): string {
  const part = result?.content?.[0];
  if (part?.type === "text" && part.text) return part.text.slice(0, 500);
  return "";
}

function terminalStatusFromSyntheticSource(source: string): SidecarToolTerminalStatus {
  switch (source) {
    case "assistant_stop_aborted":
    case "interrupt_skipped":
      return "cancelled";
    case "assistant_stop_error":
    case "assistant_stop_length":
    case "assistant_stop_skipped":
      return "failed";
    default:
      return "unknown";
  }
}

/** Map pi-agent-core tool_execution_end facts to a sidecar terminal status. */
export function terminalStatusFromExecutionEnd(event: ToolExecutionEnd): SidecarToolTerminalStatus {
  const details = event.result?.details;

  if (details?.blocked === true || details?.status === "blocked") {
    return "denied";
  }

  if (details?.__interrupted === true) {
    const source = details.source;
    if (source === "interrupt_skipped") return "cancelled";
    if (typeof source === "string" && source.includes("interrupt")) return "cancelled";
    return "unknown";
  }

  if (details?.__synthetic === true) {
    const source = typeof details.source === "string" ? details.source : undefined;
    if (!source || !SYNTHETIC_TOOL_RESULT_SOURCES.has(source)) return "unknown";
    return terminalStatusFromSyntheticSource(source);
  }

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
