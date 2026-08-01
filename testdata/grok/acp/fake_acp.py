#!/usr/bin/env python3
"""Minimal ACP stdio fake for hermetic Grok Session tests.

Speaks JSON-RPC lines on stdin/stdout. Ignores argv (the wrapper script
passes `agent --always-approve stdio`).
"""
from __future__ import annotations

import json
import os
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
            # Unattended clients set _meta.yoloMode; record for tests.
            meta = params.get("_meta") or {}
            if meta.get("yoloMode") is True:
                os.environ["FAKE_ACP_SEEN_YOLO"] = "1"
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "result": {"sessionId": session_id},
                }
            )
        elif method == "session/load":
            # FAKE_ACP_REJECT_LOAD simulates a CLI that cannot restore the
            # session (fail-closed load tests).
            if os.environ.get("FAKE_ACP_REJECT_LOAD"):
                send(
                    {
                        "jsonrpc": "2.0",
                        "id": mid,
                        "error": {"code": -32000, "message": "session not found"},
                    }
                )
            else:
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

            # Simulate shell permission prompt (Grok run_terminal_command):
            # only bash-scoped option IDs — no generic allow_always.
            # Client must reply with one of the offered allow_* IDs.
            if os.environ.get("FAKE_ACP_BASH_PERMISSION"):
                perm_id = 900001
                offered = [
                    {"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"},
                    {
                        "optionId": "allow_always_bash",
                        "name": "Always allow bash",
                        "kind": "allow_always",
                    },
                    {"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
                    {
                        "optionId": "reject_always_bash",
                        "name": "Never bash",
                        "kind": "reject_always",
                    },
                ]
                send(
                    {
                        "jsonrpc": "2.0",
                        "id": perm_id,
                        "method": "session/request_permission",
                        "params": {
                            "sessionId": sid,
                            "toolCall": {
                                "toolCallId": "tc-bash-1",
                                "title": "run_terminal_command",
                            },
                            "options": offered,
                        },
                    }
                )
                # Wait for client's permission reply (next stdin line).
                reply_line = sys.stdin.readline()
                if not reply_line:
                    send(
                        {
                            "jsonrpc": "2.0",
                            "id": mid,
                            "error": {
                                "code": -32000,
                                "message": "permission reply missing",
                            },
                        }
                    )
                    continue
                try:
                    perm_reply = json.loads(reply_line)
                except json.JSONDecodeError:
                    send(
                        {
                            "jsonrpc": "2.0",
                            "id": mid,
                            "error": {
                                "code": -32000,
                                "message": "permission reply not json",
                            },
                        }
                    )
                    continue
                outcome = (perm_reply.get("result") or {}).get("outcome") or {}
                option_id = outcome.get("optionId") or ""
                offered_ids = {o["optionId"] for o in offered}
                if option_id not in offered_ids:
                    send(
                        {
                            "jsonrpc": "2.0",
                            "id": mid,
                            "error": {
                                "code": -32000,
                                "message": (
                                    "Failed to request permission from user: "
                                    "unknown permission option for tool "
                                    f"run_terminal_command (got {option_id!r})"
                                ),
                            },
                        }
                    )
                    continue
                if not str(option_id).startswith("allow_"):
                    send(
                        {
                            "jsonrpc": "2.0",
                            "id": mid,
                            "error": {
                                "code": -32000,
                                "message": f"permission denied via {option_id}",
                            },
                        }
                    )
                    continue
                os.environ["FAKE_ACP_LAST_OPTION"] = option_id

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
