#!/usr/bin/env python3
"""Subagent correlation (2.1.170, real subscription).

Establishes how a subagent's (`Agent`/`Task` tool) `/v1/messages` requests
correlate to the parent Agent tool_call that launched them — the behavior
`internal/provider/claudetui` relies on to NEST parallel subagents instead of
rendering them as phantom main turns. Drives TWO subagents in parallel (each
runs one `echo`) and cross-references the wire + hooks.

Verifies the five facts the fix is built on:
  1. SUBAGENT WIRE TAG. A subagent's request carries header
     `X-Claude-Code-Agent-Id` (stable across its requests); main-agent
     requests do not. This is the operational discriminator + correlation key.
  2. FORWARD-LIVE JOIN (content). A subagent's first user message contains its
     Agent tool_use `input.prompt` verbatim, matching exactly ONE launch — the
     ordering-independent join used live (resolveSubagentParent).
  3. AUTHORITATIVE-BUT-LATE JOIN. `PostToolUse(Agent).tool_response.agentId`
     equals the wire agent id — structured, but only at Agent completion, so
     not usable for live nesting.
  4. SubagentStart IS NOT THE JOIN. It fires early but carries only `agent_id`
     (+ agent_type), NOT the parent tool_use_id — so it can't nest live.
  5. WIRE TOOL NAME. The launching tool_use is named `Agent` on 2.1.170
     (`Task` on older builds); the launch registry matches both.

Run (env-var driven, like the other probes — see README §"how to run"):
  go build -C proxy -o "$PRIVATE_TMP/ao-proxy" .
  "$PRIVATE_TMP/ao-proxy" --listen 127.0.0.1:8090 --log "$PRIVATE_TMP/cap.jsonl" &
  AO_BASE_URL=http://127.0.0.1:8090 AO_CAP_LOG="$PRIVATE_TMP/cap.jsonl" \
    python3 probe_subagent_correlation.py

Tiny (echo subagents) but uses the real subscription, so it consumes a little
quota. Capture goes to AO_CAP_LOG; never commit a fresh capture.
"""
import json
import os
import socket
import time
from urllib.parse import urlparse

import aoprobe

PTY_LOG = "/tmp/aohook/pty-subcorr.log"
F1, F2 = "/tmp/subcorr-1.txt", "/tmp/subcorr-2.txt"
AGENT_ID_HEADER = "x-claude-code-agent-id"

PROMPT = (
    "Launch two subagents IN PARALLEL by emitting two Task tool calls in a "
    "single message (do not wait between them). "
    "Subagent ONE: subagent_type general-purpose, prompt exactly: "
    f"'Run this one bash command and nothing else: echo SONE > {F1}'. "
    "Subagent TWO: subagent_type general-purpose, prompt exactly: "
    f"'Run this one bash command and nothing else: echo STWO > {F2}'. "
    "After BOTH subagents finish, reply with the single word DONE."
)


def wait_for_proxy(base, timeout=10.0):
    u = urlparse(base)
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection((u.hostname, u.port or 80), timeout=0.5):
                return
        except OSError:
            time.sleep(0.2)
    raise SystemExit(f"proxy at {base} never came up")


def first_user_text(body):
    for m in body.get("messages", []):
        if m.get("role") != "user":
            continue
        c = m.get("content")
        if isinstance(c, str):
            return c
        if isinstance(c, list):
            return " ".join(b.get("text", "") for b in c
                            if isinstance(b, dict) and b.get("type") == "text")
    return ""


def main():
    base = os.environ["AO_BASE_URL"]
    cap = os.environ["AO_CAP_LOG"]
    for f in (F1, F2):
        try:
            os.remove(f)
        except OSError:
            pass
    wait_for_proxy(base)

    aoprobe.seed_config(events=aoprobe.ALL_EVENTS + ["SubagentStart"], decision="allow")
    sess = aoprobe.ClaudeSession(PROMPT, base, PTY_LOG)
    sess.start()
    # Run until the MAIN turn fully ends (Stop hook), not merely until the echo
    # files appear: the Agent tool_use history + PostToolUse(Agent).agentId only
    # materialize once the subagents complete and the main agent resumes (its
    # continuation request carries the launches in its message history).
    sess.run(until=lambda: os.path.exists(F1) and os.path.exists(F2)
             and any(e.get("event") == "Stop" for e in aoprobe.payloads()), max_s=240)
    sess._drain(5.0)
    sess.exit()

    rows = aoprobe.payloads()
    recs = aoprobe.wire_records(cap)
    msg_reqs = [r for r in recs if r.get("kind") == "request" and r.get("path") == "/v1/messages"]

    # --- gather wire facts ---
    sub_first_user = {}   # agent_id -> first user text (subagent requests only)
    agent_launches = {}   # Agent/Task tool_use_id -> prompt (from main history)
    wire_tool_names = set()
    for r in msg_reqs:
        try:
            body = json.loads(r.get("body") or "{}")
        except (json.JSONDecodeError, ValueError):
            continue
        aid = next((v for k, v in r.get("headers", {}).items()
                    if k.lower() == AGENT_ID_HEADER), None)
        if aid:
            sub_first_user.setdefault(aid, first_user_text(body))
            continue
        # Main-agent request: harvest any Agent/Task launches from its history.
        for m in body.get("messages", []):
            if m.get("role") == "assistant" and isinstance(m.get("content"), list):
                for blk in m["content"]:
                    if isinstance(blk, dict) and blk.get("type") == "tool_use":
                        wire_tool_names.add(blk.get("name"))
                        if blk.get("name") in ("Agent", "Task"):
                            agent_launches[blk.get("id")] = (blk.get("input") or {}).get("prompt", "")

    # --- gather hook facts ---
    posttool_agent = {}   # tool_use_id -> agentId (from tool_response)
    substart = {}         # agent_id -> payload keys
    for e in rows:
        p = e["payload"]
        if e.get("event") == "PostToolUse" and p.get("tool_name") in ("Agent", "Task"):
            tr = p.get("tool_response") or {}
            posttool_agent[p.get("tool_use_id")] = tr.get("agentId")
        if e.get("event") == "SubagentStart":
            substart[p.get("agent_id")] = sorted(p.keys())

    # --- checks ---
    print("==== SUBAGENT CORRELATION (2.1.170) ====")
    print(f"sub1 ran: {os.path.exists(F1)}  sub2 ran: {os.path.exists(F2)}")
    ok = True

    c1 = len(sub_first_user) >= 2
    print(f"[{'PASS' if c1 else 'FAIL'}] 1. X-Claude-Code-Agent-Id on subagent wire requests: "
          f"{sorted(sub_first_user)}")
    ok &= c1

    # content match: each subagent first-user contains exactly one launch prompt
    c2 = True
    for aid, txt in sub_first_user.items():
        hits = [tid for tid, pr in agent_launches.items() if pr and pr in txt]
        uniq = len(hits) == 1
        print(f"     content-match agent_id={aid}: launches matched={hits} "
              f"({'unique' if uniq else 'AMBIGUOUS/none'})")
        c2 &= uniq
    print(f"[{'PASS' if c2 else 'FAIL'}] 2. forward-live content join is 1:1")
    ok &= c2

    # PostToolUse(Agent).agentId == one of the wire agent ids
    c3 = bool(posttool_agent) and all(a in sub_first_user for a in posttool_agent.values() if a)
    print(f"[{'PASS' if c3 else 'FAIL'}] 3. PostToolUse(Agent).tool_response.agentId == wire agent id: "
          f"{posttool_agent}")
    ok &= c3

    # SubagentStart carries agent_id but NOT a parent tool_use_id
    c4 = bool(substart) and all("tool_use_id" not in keys and "parent_tool_use_id" not in keys
                                for keys in substart.values())
    print(f"[{'PASS' if c4 else 'FAIL'}] 4. SubagentStart has agent_id, NO parent tool_use_id: "
          f"{substart}")
    ok &= c4

    c5 = "Agent" in wire_tool_names or "Task" in wire_tool_names
    print(f"[{'PASS' if c5 else 'FAIL'}] 5. wire launch tool name in {{Agent,Task}}: "
          f"{sorted(n for n in wire_tool_names if n)}")
    ok &= c5

    print(f"\n{'ALL CHECKS PASSED' if ok else 'SOME CHECKS FAILED — re-read the fix assumptions'}")
    print("========================================")
    raise SystemExit(0 if ok else 1)


if __name__ == "__main__":
    main()
