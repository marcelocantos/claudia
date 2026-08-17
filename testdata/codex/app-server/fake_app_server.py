#!/usr/bin/env python3
# Copyright 2026 Marcelo Cantos
# SPDX-License-Identifier: Apache-2.0
"""Hermetic Codex app-server. Speaks the T4.4 JSONL contract on stdio."""

from __future__ import annotations

import json
import os
import sys

REJECT_RESUME = os.environ.get("FAKE_CODEX_REJECT_RESUME") == "1"
thread_id = "thr_fake"


def emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def main() -> None:
    global thread_id
    for raw in sys.stdin:
        line = raw.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        method = msg.get("method")
        mid = msg.get("id")
        params = msg.get("params") or {}

        if method == "initialize":
            emit({"id": mid, "result": {"userAgent": "fake-codex-app-server"}})
        elif method == "initialized":
            pass
        elif method == "thread/start":
            thread_id = "thr_fake"
            model = params.get("model") or "gpt-5-codex"
            emit(
                {
                    "id": mid,
                    "result": {
                        "thread": {"id": thread_id},
                        "model": model,
                        "approvalPolicy": params.get("approvalPolicy") or "never",
                        "cwd": params.get("cwd") or "",
                        "sandbox": {"type": "readOnly"},
                    },
                }
            )
            emit({"method": "thread/started", "params": {"thread": {"id": thread_id}}})
        elif method == "thread/resume":
            tid = params.get("threadId") or ""
            known = os.environ.get("FAKE_CODEX_RESUME_ID", "")
            if REJECT_RESUME or not (str(tid).startswith("thr_") or (known and tid == known)):
                emit(
                    {
                        "id": mid,
                        "error": {"message": "no rollout found for thread id " + str(tid)},
                    }
                )
                continue
            thread_id = str(tid)
            emit(
                {
                    "id": mid,
                    "result": {
                        "thread": {"id": thread_id},
                        "model": params.get("model") or "gpt-5-codex",
                    },
                }
            )
        elif method == "turn/start":
            tid = params.get("threadId") or thread_id
            turn = "turn_success"
            emit({"id": mid, "result": {"turn": {"id": turn, "status": "in_progress"}}})
            emit(
                {
                    "method": "turn/started",
                    "params": {"threadId": tid, "turn": {"id": turn}},
                }
            )
            emit(
                {
                    "method": "item/completed",
                    "params": {
                        "threadId": tid,
                        "turnId": turn,
                        "item": {
                            "id": "item_msg",
                            "type": "agent_message",
                            "text": "Final answer.",
                        },
                    },
                }
            )
            emit(
                {
                    "method": "turn/completed",
                    "params": {
                        "threadId": tid,
                        "turn": {"id": turn, "status": "completed"},
                        "usage": {
                            "input_tokens": 10,
                            "cached_input_tokens": 4,
                            "output_tokens": 5,
                        },
                    },
                }
            )
        elif method == "turn/interrupt":
            tid = params.get("threadId") or thread_id
            turn = params.get("turnId") or "turn_interrupted"
            emit({"id": mid, "result": {}})
            emit(
                {
                    "method": "turn/completed",
                    "params": {
                        "threadId": tid,
                        "turn": {"id": turn, "status": "interrupted"},
                    },
                }
            )


if __name__ == "__main__":
    main()
