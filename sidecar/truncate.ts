// Cap a single tool result before it enters an omp seat's conversation (🎯T154).
//
// T150 compacting after the fact cannot save a seat from one result that is
// already most of the window. The bound is a fraction of the model's context
// window, with a sane absolute cap. Truncation keeps the head and the tail
// and says how much was cut and how to get the rest. The full text is not
// dropped from the host: this rewrites only the copy that becomes a tool
// result in the LLM conversation.

// Window share: one result may occupy at most 1/8 of the context window.
export const TOOL_RESULT_WINDOW_SHARE = 1 / 8;
// ~4 chars per token is the usual estimate; overestimating truncates more,
// which is the safe direction.
export const TOOL_RESULT_CHARS_PER_TOKEN = 4;
// Absolute cap: 128 KiB. The overnight incident was 673 KB per result.
export const TOOL_RESULT_ABS_CAP = 128 * 1024;
// Never shrink a bound below this, so the marker and a little payload fit.
export const TOOL_RESULT_MIN_BOUND = 2048;

// TOOL_RESULT_TRUNCATED is the scannable start of the cut marker.
export const TOOL_RESULT_TRUNCATED = "<<< truncated";

export type TruncatedToolResult = {
  text: string;
  truncated: boolean;
  original: number;
  dropped: number;
};

// toolResultBound is the per-seat char cap for one tool result.
export function toolResultBound(contextWindowTokens: number): number {
  if (contextWindowTokens <= 0) return TOOL_RESULT_ABS_CAP;
  const byWindow = Math.floor(
    contextWindowTokens * TOOL_RESULT_WINDOW_SHARE * TOOL_RESULT_CHARS_PER_TOKEN,
  );
  return Math.max(TOOL_RESULT_MIN_BOUND, Math.min(byWindow, TOOL_RESULT_ABS_CAP));
}

// truncateToolResult rewrites an oversized result for the conversation.
// `tool` is the tool name, included in the marker so the model knows what
// to re-call. A result at or under the bound is returned unchanged.
export function truncateToolResult(text: string, bound: number, tool = ""): TruncatedToolResult {
  const original = text.length;
  if (original <= bound) return { text, truncated: false, original, dropped: 0 };

  const toolBit = tool ? ` from ${tool}` : "";
  const how =
    "To get the rest: for a file, Read a narrower range; for a host tool, page with its own limit/offset.";
  // Digit width of "dropped of original" is at most 2 * digits(original) + " of ".
  const digits = String(original).length * 2 + 4;
  const markerShell = `\n${TOOL_RESULT_TRUNCATED} ${"".padEnd(digits)} chars${toolBit}; kept the head and tail. ${how} >>>\n`;
  const keep = Math.max(0, bound - markerShell.length);
  const head = Math.ceil(keep / 2);
  const tail = keep - head;
  const dropped = original - head - tail;
  const marker = `\n${TOOL_RESULT_TRUNCATED} ${dropped} of ${original} chars${toolBit}; kept the head and tail. ${how} >>>\n`;
  const out = text.slice(0, head) + marker + (tail > 0 ? text.slice(original - tail) : "");
  return { text: out, truncated: true, original, dropped };
}

// capToolText is the execute-path helper: truncate, and log that the
// conversation copy was cut so the full text is understood to remain on
// the host.
export function capToolText(text: string, bound: number, tool = ""): string {
  const capped = truncateToolResult(text, bound, tool);
  if (capped.truncated) {
    console.error(
      `sidecar: truncated ${tool || "tool"} result from ${capped.original} to ${capped.text.length} chars for the conversation (full text is not dropped from the host)`,
    );
  }
  return capped.text;
}
