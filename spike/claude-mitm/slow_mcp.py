#!/usr/bin/env python3
"""Minimal stdio MCP server that is SLOW to initialize — models a real
dispatch-container MCP (dispatch-atlassian / dispatch-gitlab) that takes a beat
to come up. Used by probe_worktree_repro.py to manufacture a >400ms PTY-silent
pause during claude's TUI boot, to test whether the composer-ready gate latches
prematurely (the worktree first-message swallow).

Sleep is on `initialize` (the first blocking handshake) by default; set
MCP_BLOCK_ON=tools to move it to tools/list instead. Duration via MCP_INIT_SLEEP
(seconds, default 2.0). Newline-delimited JSON-RPC over stdin/stdout — nothing
but protocol JSON may touch stdout.
"""
import json
import os
import sys
import time

SLEEP = float(os.environ.get("MCP_INIT_SLEEP", "2.0"))
BLOCK_ON = os.environ.get("MCP_BLOCK_ON", "init")


def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def handle(msg):
    mid = msg.get("id")
    method = msg.get("method")
    params = msg.get("params") or {}
    if method == "initialize":
        if BLOCK_ON == "init":
            time.sleep(SLEEP)
        pv = params.get("protocolVersion") or "2024-11-05"
        return {"jsonrpc": "2.0", "id": mid, "result": {
            "protocolVersion": pv,
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "slowmcp", "version": "0.0.1"},
        }}
    if method == "tools/list":
        if BLOCK_ON == "tools":
            time.sleep(SLEEP)
        return {"jsonrpc": "2.0", "id": mid, "result": {"tools": []}}
    if method == "ping":
        return {"jsonrpc": "2.0", "id": mid, "result": {}}
    if mid is None:
        return None
    return {"jsonrpc": "2.0", "id": mid,
            "error": {"code": -32601, "message": f"method not found: {method}"}}


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        resp = handle(msg)
        if resp is not None:
            send(resp)


if __name__ == "__main__":
    main()
