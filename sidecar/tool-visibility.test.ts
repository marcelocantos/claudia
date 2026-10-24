import { expect, test } from "bun:test";
import { terminalStatusFromExecutionEnd } from "./tool-visibility.ts";

test("terminal status respects isError and interrupt facts", () => {
  expect(terminalStatusFromExecutionEnd({ isError: false, result: { isError: false, content: [{ type: "text", text: "ok" }] } })).toBe(
    "completed",
  );
  expect(terminalStatusFromExecutionEnd({ isError: true, result: { content: [{ type: "text", text: "boom" }] } })).toBe("failed");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: { content: [{ type: "text", text: "Skipped due to pending steering message." }], details: { __synthetic: true } },
    }),
  ).toBe("cancelled");
  expect(
    terminalStatusFromExecutionEnd({
      isError: true,
      result: { content: [{ type: "text", text: "Tool execution was blocked" }] },
    }),
  ).toBe("denied");
  expect(terminalStatusFromExecutionEnd({})).toBe("unknown");
});
