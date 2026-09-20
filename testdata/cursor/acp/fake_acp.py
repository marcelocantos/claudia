#!/usr/bin/env python3
"""Minimal ACP stdio fake for hermetic Cursor Session tests.

Speaks JSON-RPC lines on stdin/stdout. Ignores argv (the wrapper script
passes `agent --force --trust --approve-mcps acp`).
"""
from __future__ import annotations

import json
import os
import sys
import time


def send(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def main() -> None:
    session_id = "sess-fake-cursor-1"
    authed = False
    held = None
    swallowed_first_prompt = False
    slow_prompt = None
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

        if path := os.environ.get("FAKE_ACP_REQUEST_LOG"):
            with open(path, "a", encoding="utf-8") as log:
                log.write(json.dumps({"pid": os.getpid(), "method": method}) + "\n")
        if method == os.environ.get("FAKE_ACP_WITHHOLD"):
            continue

        if method == "initialize":
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "result": {
                        "protocolVersion": 1,
                        "agentCapabilities": {"loadSession": True},
                        "authMethods": [{"id": "cursor_login", "name": "Cursor Login"}],
                    },
                }
            )
        elif method == "authenticate":
            if (params.get("methodId") or "") != "cursor_login":
                send(
                    {
                        "jsonrpc": "2.0",
                        "id": mid,
                        "error": {"code": -32000, "message": "unknown auth method"},
                    }
                )
                continue
            authed = True
            send({"jsonrpc": "2.0", "id": mid, "result": {}})
        elif method == "session/cancel":
            if held is not None:
                send({"jsonrpc": "2.0", "id": held, "result": {"stopReason": "cancelled"}})
                held = None
        elif method == "notifications/initialized":
            pass
        elif method == "session/new":
            if not authed:
                send(
                    {
                        "jsonrpc": "2.0",
                        "id": mid,
                        "error": {"code": -32000, "message": "not authenticated"},
                    }
                )
                continue
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "result": {"sessionId": session_id},
                }
            )
        elif method == "session/load":
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
                result = {"sessionId": sid}
                if os.environ.get("FAKE_ACP_HUGE_LOAD"):
                    # Larger than the old 1 MiB Scan cap (jevons 🎯T545).
                    result["replay"] = "x" * (2 * 1024 * 1024 + 64)
                send(
                    {
                        "jsonrpc": "2.0",
                        "id": mid,
                        "result": result,
                    }
                )
        elif method == "session/prompt":
            sid = params.get("sessionId") or session_id

            # 🎯T83 shapes. SWALLOW_FIRST_PROMPT drops exactly one
            # delivery and then behaves: the peer that loses a brief but
            # answers the harness's re-issue. PROMPT_DELAY_MS is the
            # slow-but-healthy peer, which a bound must never call stuck.
            if os.environ.get("FAKE_ACP_SWALLOW_FIRST_PROMPT") and not swallowed_first_prompt:
                swallowed_first_prompt = True
                continue

            # 🎯T92: the peer that was merely slow, not deaf. It holds the
            # opening delivery without saying anything, waits for the
            # harness to give up and re-deliver, and then answers the
            # delivery the harness ABANDONED. Nothing here watches a
            # clock: the re-delivery is the signal, and the harness
            # causes it. A client that drops the abandoned id leaves its
            # caller with the reply on the wire and no terminal event to
            # stop waiting for.
            if os.environ.get("FAKE_ACP_ANSWER_ABANDONED"):
                if slow_prompt is None:
                    slow_prompt = mid
                    continue
                answer_id, slow_prompt = slow_prompt, None
                send(
                    {
                        "jsonrpc": "2.0",
                        "method": "session/update",
                        "params": {
                            "sessionId": sid,
                            "update": {
                                "sessionUpdate": "agent_message_chunk",
                                "content": {"type": "text", "text": "pong"},
                            },
                        },
                    }
                )
                send(
                    {
                        "jsonrpc": "2.0",
                        "id": answer_id,
                        "result": {"stopReason": "end_turn"},
                    }
                )
                continue
            if delay_ms := os.environ.get("FAKE_ACP_PROMPT_DELAY_MS"):
                time.sleep(int(delay_ms) / 1000.0)
            text = ""
            for block in params.get("prompt") or []:
                if isinstance(block, dict) and block.get("type") == "text":
                    text = block.get("text") or ""
                    break

            if os.environ.get("FAKE_ACP_STEER"):
                # 🎯T72.1 steer fake: hold the first prompt open (stream one
                # chunk, no result). A second session/prompt while held is a
                # steer: settle the held id as cancelled, then answer the new
                # id with text that reflects the steer. session/cancel while
                # held settles it as cancelled with no new turn.
                if held is None:
                    held = mid
                    send(
                        {
                            "jsonrpc": "2.0",
                            "method": "session/update",
                            "params": {
                                "sessionId": sid,
                                "update": {
                                    "sessionUpdate": "agent_message_chunk",
                                    "content": {"type": "text", "text": "drafting the essay"},
                                },
                            },
                        }
                    )
                    continue
                send({"jsonrpc": "2.0", "id": held, "result": {"stopReason": "cancelled"}})
                held = None
                send(
                    {
                        "jsonrpc": "2.0",
                        "method": "session/update",
                        "params": {
                            "sessionId": sid,
                            "update": {
                                "sessionUpdate": "agent_message_chunk",
                                "content": {"type": "text", "text": "STEERED: " + text},
                            },
                        },
                    }
                )
                send({"jsonrpc": "2.0", "id": mid, "result": {"stopReason": "end_turn"}})
                continue

            if os.environ.get("FAKE_ACP_CURSOR_PERMISSION"):
                perm_id = 900001
                offered = [
                    {"optionId": "allow-once", "name": "Allow once", "kind": "allow-once"},
                    {"optionId": "allow-always", "name": "Allow always", "kind": "allow-always"},
                    {"optionId": "reject-once", "name": "Reject", "kind": "reject-once"},
                ]
                send(
                    {
                        "jsonrpc": "2.0",
                        "id": perm_id,
                        "method": "session/request_permission",
                        "params": {
                            "sessionId": sid,
                            "toolCall": {
                                "toolCallId": "tc-1",
                                "title": "Shell",
                            },
                            "options": offered,
                        },
                    }
                )
                reply_line = sys.stdin.readline()
                if not reply_line:
                    send(
                        {
                            "jsonrpc": "2.0",
                            "id": mid,
                            "error": {"code": -32000, "message": "permission reply missing"},
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
                            "error": {"code": -32000, "message": "permission reply not json"},
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
                                "message": f"unknown permission option (got {option_id!r})",
                            },
                        }
                    )
                    continue
                if not str(option_id).startswith("allow"):
                    send(
                        {
                            "jsonrpc": "2.0",
                            "id": mid,
                            "error": {"code": -32000, "message": f"permission denied via {option_id}"},
                        }
                    )
                    continue
                os.environ["FAKE_ACP_LAST_OPTION"] = option_id

            if os.environ.get("FAKE_ACP_CURSOR_ASK"):
                ask_id = 900002
                send(
                    {
                        "jsonrpc": "2.0",
                        "id": ask_id,
                        "method": "cursor/ask_question",
                        "params": {
                            "toolCallId": "call_ask",
                            "questions": [
                                {
                                    "id": "q1",
                                    "prompt": "Continue?",
                                    "options": [{"id": "yes", "label": "Yes"}],
                                }
                            ],
                        },
                    }
                )
                _ = sys.stdin.readline()

            reply = "pong" if "pong" in text.lower() or text else "ok"
            # A real agent streams a reply as token deltas, so the word
            # "pong" can arrive as "p" then "ong". FAKE_ACP_CHUNKS is a
            # "|"-separated delta list that reproduces that split
            # (🎯T79); unset, the reply goes out whole as before.
            chunks = (os.environ.get("FAKE_ACP_CHUNKS") or reply).split("|")
            for chunk in chunks:
                send(
                    {
                        "jsonrpc": "2.0",
                        "method": "session/update",
                        "params": {
                            "sessionId": sid,
                            "update": {
                                "sessionUpdate": "agent_message_chunk",
                                "content": {"type": "text", "text": chunk},
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
                        "usage": {"inputTokens": 10, "outputTokens": 2},
                    },
                }
            )
        elif mid is not None and method:
            send(
                {
                    "jsonrpc": "2.0",
                    "id": mid,
                    "error": {"code": -32601, "message": f"unknown {method}"},
                }
            )


if __name__ == "__main__":
    main()
