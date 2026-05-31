#!/usr/bin/env python3
"""Minimal stdio MCP server for the AO hook-channel MCP probe.

One tool, `ping`, returning a fixed token (or an `isError` result when asked to
fail). Enough to prove Claude Code surfaces MCP tool calls through the hook
channel — PreToolUse/PostToolUse fire with tool_name `mcp__<server>__ping`, and
the success result / error is recoverable from PostToolUse. The hook mechanics
are MCP-transport-agnostic, so a remote server (context7 etc.) behaves
identically at this layer; a local stdio server just removes network flakiness.

Transport is newline-delimited JSON-RPC over stdin/stdout (the MCP stdio
transport). NOTHING but protocol JSON may go to stdout — stray prints corrupt
the stream and the client disconnects.
"""
import json
import sys

TOOLS = [{
    "name": "ping",
    "description": ("Return a fixed token. Call this when asked to ping. Pass "
                    "`echo` to include text in the reply; pass `fail`=true to "
                    "force an error result."),
    "inputSchema": {
        "type": "object",
        "properties": {
            "echo": {"type": "string", "description": "text to echo back"},
            "fail": {"type": "boolean", "description": "force an error result"},
        },
        "required": [],
    },
}]


def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def handle(msg):
    mid = msg.get("id")
    method = msg.get("method")
    params = msg.get("params") or {}
    if method == "initialize":
        # Echo the client's negotiated protocol version — hardcoding an older
        # one makes a newer client silently disconnect (no tool ever fires).
        pv = params.get("protocolVersion") or "2024-11-05"
        return {"jsonrpc": "2.0", "id": mid, "result": {
            "protocolVersion": pv,
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "aoprobe", "version": "0.0.1"},
        }}
    if method == "tools/list":
        return {"jsonrpc": "2.0", "id": mid, "result": {"tools": TOOLS}}
    if method == "tools/call":
        args = params.get("arguments") or {}
        if args.get("fail"):
            return {"jsonrpc": "2.0", "id": mid, "result": {
                "content": [{"type": "text", "text": "PING-FAILED (forced error)"}],
                "isError": True,
            }}
        return {"jsonrpc": "2.0", "id": mid, "result": {
            "content": [{"type": "text", "text": "PONG:" + str(args.get("echo", ""))}],
            "isError": False,
        }}
    if method == "ping":                       # MCP keepalive
        return {"jsonrpc": "2.0", "id": mid, "result": {}}
    if mid is None:                            # a notification — no response
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
