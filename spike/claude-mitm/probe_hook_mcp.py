#!/usr/bin/env python3
"""Probe: does Claude Code surface MCP tool calls through the hook channel, can
AO gate them per-call, and is the completion (result / error) recoverable?

Closes the "MCP tools" open question. A minimal local stdio MCP server
(mcp_server.py) exposes one tool, `ping`. The hook MECHANICS are MCP-transport-
agnostic, so this generalizes to remote servers (context7 etc.); a local server
just removes network flakiness.

NAMING is the one thing that varies and that AO writes code against: a user-scope
`mcpServers` entry surfaces as `mcp__<server>__<tool>` (here `mcp__aoprobe__ping`),
but a PLUGIN-provided server (e.g. the user's context7) surfaces as
`mcp__plugin_<plugin>_<server>__<tool>` (observed live this session as
`mcp__plugin_context7_context7__query-docs`). AO must match the `mcp__` prefix, not
assume a bare server segment. This probe drives the local server only; the plugin
name shape is confirmed from the live tool list, not driven end-to-end here.

Two launches:
  allow: ping(echo=HELLO) then ping(fail=true) -> PreToolUse fires for BOTH
    (tool_name starts `mcp__`); the success call completes on PostToolUse
    (PONG:HELLO) and the fail=true call on PostToolUseFailure (isError /
    PING-FAILED) — the SAME success/failure split as Bash, asserted per-event,
    validating both success- and failure-completion info for MCP tools.
  deny: ping(echo=WORLD) with relay deny -> PreToolUse fires, tool blocked (no
    PostToolUse) -> per-call permission gating works for MCP tools too.
"""
import json
import os

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
MCP_SERVER = f"{aoprobe.SPIKE_DIR}/mcp_server.py"
MCP = {"aoprobe": {"type": "stdio", "command": "python3", "args": [MCP_SERVER]}}

ALLOW_PROMPT = ("Call the ping tool from the aoprobe MCP server with echo set to "
                "HELLO. Then call the ping tool again with fail set to true. Then "
                "reply DONE and stop. Do not use any other tool.")
DENY_PROMPT = ("Call the ping tool from the aoprobe MCP server with echo set to "
               "WORLD, then reply DONE and stop. Do not use any other tool.")


def mcp_pre():
    return [e for e in aoprobe.payloads() if e.get("event") == "PreToolUse"
            and str(e.get("tool")).startswith("mcp__")]


def mcp_post():
    # Either completion event: success -> PostToolUse, error (MCP isError) ->
    # PostToolUseFailure (same family as a non-zero Bash exit).
    return [e for e in aoprobe.payloads()
            if e.get("event") in ("PostToolUse", "PostToolUseFailure")
            and str(e.get("tool")).startswith("mcp__")]


def run(name, prompt, decision, want_calls):
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, decision=decision, mcp_servers=MCP)
    log = f"{aoprobe.AOHOOK}/pty-mcp-{name}.log"
    sess = aoprobe.ClaudeSession(prompt, BASE_URL, log)
    sess.start()
    # The MCP server must spin up + connect; give it room. Stop once we've seen
    # the expected number of mcp PreToolUse calls, then drain for PostToolUse.
    sess.run(until=lambda: len(mcp_pre()) >= want_calls, max_s=90)
    sess._drain(5.0)
    sess.exit()

    pre, post = mcp_pre(), mcp_post()
    if not pre:
        try:
            tail = open(log, "rb").read()[-1400:].decode(errors="replace")
        except OSError:
            tail = "(no log)"
        print(f"   [diag] no mcp__ PreToolUse seen — PTY tail:\n{tail}")
    return {"pre": pre, "post": post,
            "tool_name": (pre[0].get("tool") if pre else None), "log": log}


def main():
    print("==== MCP TOOL HOOK-CHANNEL PROBE ====")

    print("\n[allow] ping(echo=HELLO) then ping(fail=true)")
    a = run("allow", ALLOW_PROMPT, "allow", want_calls=2)
    comp = [(e["event"], json.dumps(e["payload"])) for e in a["post"]]
    # Route by EVENT, not "appears somewhere": the success result must land on a
    # PostToolUse, and the isError result on a PostToolUseFailure — the same
    # success/failure split confirmed for Bash, now asserted per-event for MCP.
    success_on_post = any(ev == "PostToolUse" and "PONG:HELLO" in t for ev, t in comp)
    error_on_fail = any(ev == "PostToolUseFailure"
                        and (("PING-FAILED" in t) or ('"iserror": true' in t.lower())
                             or ('"is_error": true' in t.lower()))
                        for ev, t in comp)
    print(f"   tool_name surfaced: {a['tool_name']}")
    print(f"   PreToolUse mcp calls: {len(a['pre'])}   completion events: {[ev for ev, _ in comp]}")
    print(f"   success result carried on PostToolUse (PONG:HELLO): {success_on_post}")
    print(f"   isError result carried on PostToolUseFailure: {error_on_fail}")
    if comp:
        print(f"   sample completion payload: {comp[0][1][:320]}")

    print("\n[deny] ping(echo=WORLD) with relay deny")
    d = run("deny", DENY_PROMPT, "deny", want_calls=1)
    blocked = len(d["pre"]) >= 1 and len(d["post"]) == 0
    print(f"   tool_name surfaced: {d['tool_name']}")
    print(f"   PreToolUse mcp calls: {len(d['pre'])}   completion events: {len(d['post'])}")
    print(f"   gated (PreToolUse fired, no completion = blocked): {blocked}")

    ok = (a["tool_name"] and str(a["tool_name"]).startswith("mcp__")
          and len(a["pre"]) >= 2 and success_on_post and error_on_fail and blocked)
    print(f"\nVERDICT: {'CONFIRMED — MCP tools fire hooks, gate per-call, and report success+error' if ok else 'PARTIAL/NOT — inspect above'}")
    print("=====================================")


if __name__ == "__main__":
    main()
