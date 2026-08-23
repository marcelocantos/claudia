#!/usr/bin/env python3
"""Minimal Cursor --print stream-json fake for hermetic Task tests."""

from __future__ import annotations

import json
import sys


def emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def main() -> int:
    # Argv looks like: … --print --output-format stream-json --force --trust [--model X] [--resume ID] PROMPT
    prompt = sys.argv[-1] if sys.argv else ""
    sid = "fake-cursor-task-session"
    if "--resume" in sys.argv:
        i = sys.argv.index("--resume")
        if i + 1 < len(sys.argv) - 1:
            sid = sys.argv[i + 1]

    emit(
        {
            "type": "system",
            "subtype": "init",
            "session_id": sid,
            "model": "fake-cursor",
        }
    )
    emit(
        {
            "type": "assistant",
            "message": {
                "role": "assistant",
                "content": [{"type": "text", "text": "pong"}],
            },
            "session_id": sid,
        }
    )
    emit(
        {
            "type": "result",
            "subtype": "success",
            "is_error": False,
            "duration_ms": 12,
            "result": "pong",
            "session_id": sid,
            "usage": {
                "inputTokens": 10,
                "outputTokens": 2,
                "cacheReadTokens": 1,
                "cacheWriteTokens": 0,
            },
        }
    )
    _ = prompt
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
