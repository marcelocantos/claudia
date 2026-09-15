#!/usr/bin/env python3
# Copyright 2026 Marcelo Cantos
# SPDX-License-Identifier: Apache-2.0
"""Hermetic Codex app-server. Speaks the T4.4 JSONL contract on stdio."""

from __future__ import annotations

import json
import os
import signal
import sys

REJECT_RESUME = os.environ.get("FAKE_CODEX_REJECT_RESUME") == "1"
# FAKE_CODEX_STEER=1: the CLI "has" turn/steer — the schema probe finds
# v2/TurnSteerParams.json and the method answers. Unset, the probe finds
# nothing and turn/steer is refused, like a CLI that predates it.
STEER = os.environ.get("FAKE_CODEX_STEER") == "1"
# FAKE_CODEX_HOLD_TURN=1: turn/start does not complete on its own; the
# turn stays in flight until turn/steer or turn/interrupt lands, so a
# test can observe the in_turn phase.
HOLD_TURN = os.environ.get("FAKE_CODEX_HOLD_TURN") == "1"
thread_id = "thr_fake"


def _flush_home(reason: str) -> None:
    home = os.environ.get("CODEX_HOME") or ""
    if not home:
        return
    os.makedirs(home, exist_ok=True)
    with open(os.path.join(home, "flushed"), "w", encoding="utf-8") as fh:
        fh.write(reason)


def _write_rollout(tid: str) -> None:
    home = os.environ.get("CODEX_HOME") or ""
    if not home or not tid:
        return
    sess = os.path.join(home, "sessions", tid)
    os.makedirs(sess, exist_ok=True)
    path = os.path.join(sess, "rollout.json")
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(json.dumps({"id": tid}))


def _has_rollout(tid: str) -> bool:
    home = os.environ.get("CODEX_HOME") or ""
    if not home or not tid:
        return False
    legacy = os.path.join(home, "sessions", tid, "rollout.json")
    if os.path.isfile(legacy) and os.path.getsize(legacy) > 0:
        return True
    root = os.path.join(home, "sessions")
    if not os.path.isdir(root):
        return False
    for dirpath, _dirs, files in os.walk(root):
        for name in files:
            if tid in name and (name.endswith(".jsonl") or name.endswith(".json")):
                path = os.path.join(dirpath, name)
                if os.path.isfile(path) and os.path.getsize(path) > 0:
                    return True
    return False


def _on_term(_signum: int, _frame: object) -> None:
    _flush_home("sigterm")
    sys.exit(0)


signal.signal(signal.SIGTERM, _on_term)


def emit(obj: dict) -> None:
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def _generate_json_schema(argv: list[str]) -> None:
    """`codex app-server generate-json-schema --out DIR`: the probe claudia
    runs at Start to learn whether this CLI lists turn/steer."""
    out = ""
    for i, arg in enumerate(argv):
        if arg == "--out" and i + 1 < len(argv):
            out = argv[i + 1]
    if not out:
        sys.exit(2)
    os.makedirs(os.path.join(out, "v2"), exist_ok=True)
    with open(os.path.join(out, "v2", "TurnStartParams.json"), "w", encoding="utf-8") as fh:
        fh.write(json.dumps({"title": "TurnStartParams"}))
    if STEER:
        with open(os.path.join(out, "v2", "TurnSteerParams.json"), "w", encoding="utf-8") as fh:
            fh.write(json.dumps({"title": "TurnSteerParams"}))


def _complete_turn(tid: str, turn: str) -> None:
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


def main() -> None:
    global thread_id
    if "generate-json-schema" in sys.argv:
        _generate_json_schema(sys.argv)
        return
    steer_log = os.environ.get("FAKE_CODEX_STEER_LOG")
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
            home_log = os.environ.get("FAKE_CODEX_LAST_HOME")
            if home_log:
                with open(home_log, "w", encoding="utf-8") as fh:
                    fh.write(os.environ.get("CODEX_HOME", ""))
            emit({"id": mid, "result": {"userAgent": "fake-codex-app-server"}})
        elif method == "initialized":
            pass
        elif method == "thread/start":
            if os.environ.get("FAKE_CODEX_HANG_START") == "1":
                continue
            thread_id = "thr_fake"
            model = params.get("model") or "gpt-5-codex"
            last = os.environ.get("FAKE_CODEX_LAST_START")
            if last:
                with open(last, "w", encoding="utf-8") as fh:
                    fh.write(json.dumps(params, separators=(",", ":")))
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
            home = os.environ.get("CODEX_HOME") or ""
            if home and os.environ.get("FAKE_CODEX_SKIP_ROLLOUT") != "1":
                _write_rollout(thread_id)
        elif method == "thread/name/set":
            tid = params.get("threadId") or thread_id
            thread_id = str(tid)
            emit({"id": mid, "result": {}})
            # Live Codex persists session_meta here, not on thread/start
            # (jevons 🎯T545.1.3).
            _write_rollout(thread_id)
        elif method == "thread/resume":
            tid = params.get("threadId") or ""
            known = os.environ.get("FAKE_CODEX_RESUME_ID", "")
            if REJECT_RESUME or not (
                str(tid).startswith("thr_")
                or (known and tid == known)
                or _has_rollout(str(tid))
            ):
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
            if not HOLD_TURN:
                _complete_turn(tid, turn)
        elif method == "turn/steer":
            tid = params.get("threadId") or thread_id
            expected = params.get("expectedTurnId") or ""
            if not STEER:
                emit({"id": mid, "error": {"code": -32601, "message": "Method not found: turn/steer"}})
                continue
            if steer_log:
                with open(steer_log, "a", encoding="utf-8") as fh:
                    fh.write(json.dumps(params, separators=(",", ":")) + "\n")
            if expected != "turn_success":
                emit({"id": mid, "error": {"message": "turn " + expected + " is not the active turn"}})
                continue
            emit({"id": mid, "result": {"turnId": expected}})
            _complete_turn(tid, expected)
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

    # Live Codex stays up after stdin EOF until SIGTERM flushes sqlite.
    # Exit-on-EOF would hide a Close() that only Kill-s (🎯T545.1.2).
    signal.pause()


if __name__ == "__main__":
    main()
