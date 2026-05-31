#!/usr/bin/env python3
"""Probe: which hook events fire for a Task subagent and a backgrounded Bash,
and do subagent-inner tool calls surface (and carry parent correlation)?

This resolves the "background-task / subagent lifecycle" question: in stream-json
mode AO gets task_started/task_updated/task_notification + parent_tool_use_id.
None of those are on the wire or in the transcript. Can hooks recover the
lifecycle (start/end) and the parent linkage?

Auto-allows every tool via the relay; registers all lifecycle hooks; prompts for
a subagent + a run_in_background Bash; captures every hook payload.
"""
import json
import os
import time

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
BG = "/tmp/aohook/bg.txt"
PTY_LOG = "/tmp/aohook/pty-coverage.log"

PROMPT = (
    "Do exactly these two steps, then reply DONE and stop:\n"
    "1. Use the Task tool to launch a subagent (subagent_type general-purpose) "
    "whose prompt is exactly: 'Run the bash command: echo SUBAGENT-RAN — then "
    "report the single word you printed.'\n"
    f"2. Use the Bash tool with run_in_background=true to run: sleep 5; echo BG-DONE > {BG}"
)


def main():
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, decision="allow", sleep_s=0.0)
    try:
        os.remove(BG)
    except OSError:
        pass

    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()
    # run until the backgrounded bash finished (proves we can observe completion),
    # then drain a few extra seconds for any late SubagentStop / PostToolUse.
    sess.run(until=lambda: os.path.exists(BG), max_s=160)
    sess._drain(4.0)
    sess.exit()

    rows = aoprobe.payloads()
    print("==== HOOK COVERAGE RESULT ====")
    print(f"bg.txt exists (bg bash completed): {os.path.exists(BG)}")
    print(f"total hook events captured: {len(rows)}")
    print("\n-- event timeline (event, tool, tool_use_id, parent keys) --")
    for e in rows:
        p = e.get("payload", {})
        parent = {k: p[k] for k in p if "parent" in k.lower() or k in
                  ("agent_type", "subagent_type", "agentType")}
        print(f"  {e.get('event'):<16} tool={str(e.get('tool')):<10} "
              f"tuid={p.get('tool_use_id','')[:24]:<24} extra={parent}")
    # show the full SubagentStop / Stop / Notification payloads (the lifecycle signals)
    for want in ("SubagentStop", "Stop", "Notification"):
        for e in rows:
            if e.get("event") == want:
                print(f"\n-- full {want} payload --")
                print(json.dumps(e.get("payload", {}), indent=2)[:1200])
                break
    print("\npty log:", PTY_LOG)
    print("==============================")


if __name__ == "__main__":
    main()
