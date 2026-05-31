#!/usr/bin/env python3
"""Probe: are ExitPlanMode and AskUserQuestion interceptable via PreToolUse?

If yes, AO drives plan-approval and detects/handles structured questions through
the SAME hook channel as permissions — no shift+tab cycling, no Select-widget
keystroke driving (the fragile parts of the original spike spec).

modes:
  plan -> launch in --permission-mode plan; model researches then calls
          ExitPlanMode. Hook ALLOWS (approve). Capture the plan text from
          tool_input and whether/what executes after approval (+ permission_mode).
  ask  -> model calls AskUserQuestion. Hook DENIES. Capture the question/options
          from tool_input (proves AO can read them + render its own UI) and how
          the model reacts to a denied question.

Usage: probe_hook_special.py <plan|ask>
"""
import json
import os
import sys

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
MODE = sys.argv[1] if len(sys.argv) > 1 else "plan"

if MODE == "plan":
    DECISION = "allow"
    EXTRA = ["--permission-mode", "plan"]
    PROMPT = ("Make a short plan to create a file /tmp/aohook/plan_out.txt containing "
              "the word hi. Present it with the ExitPlanMode tool. Once approved, do it.")
    WANT = "ExitPlanMode"
else:
    DECISION = "deny"
    EXTRA = []
    PROMPT = ("Use the AskUserQuestion tool to ask me to choose the filename: "
              "option A is 'alpha.txt', option B is 'beta.txt'. Ask exactly one question.")
    WANT = "AskUserQuestion"

PTY_LOG = f"/tmp/aohook/pty-special-{MODE}.log"


def saw(tool_substr):
    return any(tool_substr.lower() in str(e.get("tool", "")).lower()
               for e in aoprobe.payloads() if e.get("event") == "PreToolUse")


def main():
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, decision=DECISION, sleep_s=0.0)
    try:
        os.remove("/tmp/aohook/plan_out.txt")
    except OSError:
        pass
    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG, extra_args=EXTRA)
    sess.start()
    # run until the target special tool is intercepted, then drain to see the aftermath
    sess.run(until=lambda: saw(WANT), max_s=150)
    sess._drain(6.0)
    sess.exit()

    rows = aoprobe.payloads()
    print(f"==== SPECIAL-TOOL PROBE ({MODE}) ====")
    print("timeline:", [(e.get("event"), e.get("tool")) for e in rows])
    for e in rows:
        if e.get("event") == "PreToolUse" and WANT.lower() in str(e.get("tool", "")).lower():
            p = e["payload"]
            print(f"\n-- intercepted {p.get('tool_name')} (permission_mode={p.get('permission_mode')}) --")
            print("tool_input:", json.dumps(p.get("tool_input"), indent=2)[:1400])
            break
    if MODE == "plan":
        print("\nplan_out.txt exists (executed after approval):",
              os.path.exists("/tmp/aohook/plan_out.txt"))
        # what permission_mode did post-approval tool calls run under?
        post = [e["payload"].get("permission_mode") for e in rows
                if e.get("event") == "PreToolUse"]
        print("permission_mode sequence across PreToolUse:", post)
    if MODE == "ask":
        # how did the model react to the denied question? show the Stop last message
        for e in reversed(rows):
            if e.get("event") in ("Stop", "SubagentStop"):
                print("\nlast_assistant_message after deny:",
                      repr(e["payload"].get("last_assistant_message"))[:300]); break
    print("pty log:", PTY_LOG)
    print("=====================================")


if __name__ == "__main__":
    main()
