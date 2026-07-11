#!/usr/bin/env python3
"""Minimal ACP stdio fake for hermetic Grok Session tests.

Speaks JSON-RPC lines on stdin/stdout. Ignores argv (the wrapper script
passes `agent --always-approve stdio`).
"""
from __future__ import annotations

import json
import sys


def send(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def main() -> None:
    session_id = "sess-fake-acp-1"
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        mid = msg.get("id")
        method = msg.get("method") or ""
        params = msg.get("params") or {}

        if method == "initialize":
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "result": {
                        "protocolVersion": 1,
                        "agentCapabilities": {"loadSession": True},
                    },
                }
            )
        elif method in ("notifications/initialized", "session/cancel"):
            pass
        elif method == "session/new":
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "result": {"sessionId": session_id},
                }
            )
        elif method == "session/load":
            sid = params.get("sessionId") or session_id
            session_id = sid
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "result": {"sessionId": sid},
                }
            )
        elif method == "session/prompt":
            sid = params.get("sessionId") or session_id
            text = ""
            for block in params.get("prompt") or []:
                if isinstance(block, dict) and block.get("type") == "text":
                    text = block.get("text") or ""
                    break
            # Stream a thought (ignored by client for WaitForResponse text)
            # and a message chunk, then complete.
            send(
                {
                    "jsonrpc": "2.0",
                    "method": "session/update",
                    "params": {
                        "sessionId": sid,
                        "update": {
                            "sessionUpdate": "agent_thought_chunk",
                            "content": {"type": "text", "text": "thinking"},
                        },
                    },
                }
            )
            reply = "pong" if "pong" in text.lower() or text else "ok"
            send(
                {
                    "jsonrpc": "2.0",
                    "method": "session/update",
                    "params": {
                        "sessionId": sid,
                        "update": {
                            "sessionUpdate": "agent_message_chunk",
                            "content": {"type": "text", "text": reply},
                        },
                    },
                }
            )
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "result": {
                        "stopReason": "end_turn",
                        "_meta": {
                            "inputTokens": 10,
                            "outputTokens": 2,
                            "cachedReadTokens": 0,
                        },
                    },
                }
            )
        elif mid is not None and method:
            # Unknown server→client would not appear here; reply error for oddities.
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "error": {"code": -32601, "message": f"unknown {method}"},
                }
            )


if __name__ == "__main__":
    main()
