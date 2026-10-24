// 🎯T221: Cursor native exec handlers emit tool_start before a shell
// finishes and tool_end after, with the same call_id (jevons 🎯T1218).

import { expect, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { cursorExecHandlers, type CursorExecVisibility } from "./cursorexec.ts";

type VisEvent = { phase: "start" | "end"; call_id: string; name: string; status?: string; at: number };

test("native shell emits tool_start before completion and tool_end after with same call_id", async () => {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "cursorexec-vis-")));
  const trace: VisEvent[] = [];
  const visibility: CursorExecVisibility = {
    toolStart(call_id, name) {
      trace.push({ phase: "start", call_id, name, at: Date.now() });
    },
    toolEnd(call_id, name, _text, status) {
      trace.push({ phase: "end", call_id, name, status, at: Date.now() });
    },
  };
  const h = cursorExecHandlers(() => dir, () => 100_000, undefined, undefined, undefined, () => visibility) as any;
  const callId = "native-shell-1";
  const run = h.shell({ toolCallId: callId, command: "sleep 0.05 && echo ok", timeout: 5 });
  const start = trace.find((e) => e.phase === "start");
  expect(start).toBeDefined();
  expect(start!.call_id).toBe(callId);
  expect(start!.name).toBe("bash");
  await run;
  const end = trace.find((e) => e.phase === "end");
  expect(end).toBeDefined();
  expect(end!.call_id).toBe(callId);
  expect(end!.status).toBe("completed");
  expect(start!.at).toBeLessThan(end!.at);
});

test("piBash shares the same visibility path", async () => {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "cursorexec-pibash-")));
  let sawStart = false;
  const visibility: CursorExecVisibility = {
    toolStart() {
      sawStart = true;
    },
    toolEnd(_call_id, _name, _text, status) {
      expect(status).toBe("completed");
    },
  };
  const h = cursorExecHandlers(() => dir, () => 100_000, undefined, undefined, undefined, () => visibility) as any;
  await h.piBash({ toolCallId: "pibash-1", args: { command: "true", timeout: 5 } });
  expect(sawStart).toBe(true);
});
