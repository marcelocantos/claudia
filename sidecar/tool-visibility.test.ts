import { expect, test } from "bun:test";
import { terminalStatusFromExecutionEnd } from "./tool-visibility.ts";

test("terminal status respects isError and typed interrupt/synthetic facts", () => {
  expect(terminalStatusFromExecutionEnd({ isError: false, result: { isError: false, content: [{ type: "text", text: "ok" }] } })).toBe(
    "completed",
  );
  expect(terminalStatusFromExecutionEnd({ isError: true, result: { content: [{ type: "text", text: "boom" }] } })).toBe("failed");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: {
        content: [{ type: "text", text: "looks fine" }],
        isError: true,
      },
    }),
  ).toBe("failed");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: {
        content: [{ type: "text", text: "Skipped due to pending steering message." }],
        details: { __synthetic: true, source: "interrupt_skipped", executed: false },
      },
    }),
  ).toBe("cancelled");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: {
        content: [{ type: "text", text: "Skipped due to pending steering message." }],
        details: { __synthetic: true },
      },
    }),
  ).toBe("unknown");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: {
        content: [{ type: "text", text: "upstream blew up" }],
        details: { __synthetic: true, source: "assistant_stop_error", executed: false },
      },
    }),
  ).toBe("failed");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: {
        content: [{ type: "text", text: "truncated args" }],
        details: { __synthetic: true, source: "assistant_stop_length", executed: false },
      },
    }),
  ).toBe("failed");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: { content: [{ type: "text", text: "Tool execution was blocked" }] },
    }),
  ).toBe("failed");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: { content: [{ type: "text", text: "policy" }], details: { blocked: true } },
    }),
  ).toBe("denied");
  expect(terminalStatusFromExecutionEnd({})).toBe("unknown");
});
