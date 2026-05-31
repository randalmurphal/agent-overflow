#!/usr/bin/env python3
"""Probe: do we get PROPER completion signals + info for a backgrounded subagent
AND a backgrounded Bash — via DISTINCT channels?

  - the SUBAGENT completes via the `SubagentStop` HOOK, which should carry
    agent_id + agent_transcript_path + last_assistant_message.
  - the backgrounded BASH completes via the WIRE: a `<task-notification>` user
    message flushed into the NEXT /v1/messages request (the
    enqueuePendingNotification channel — NOT a hook). So we MUST drive a
    follow-up turn after the bg bash finishes, or the signal never reaches the
    wire and we'd wrongly read it as absent.

Also captures the dispatch-time `running in background` tool_response and the
turn-completion `Stop` hook. The two completion paths are reported separately —
they do not blur into one signal.
"""
import json
import os
import re
import time

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
CAP = os.environ.get("AO_CAP_LOG", "/tmp/ao-cap.jsonl")
BG = f"{aoprobe.AOHOOK}/bgc.txt"
PTY_LOG = f"{aoprobe.AOHOOK}/pty-bgcomplete.log"

PROMPT = (
    "Do exactly these two steps, then reply STEP1-DONE and stop:\n"
    "1. Use the Task tool to launch a subagent (subagent_type general-purpose) "
    "whose prompt is exactly: 'Run the bash command: echo SUBAGENT-RAN — then "
    "reply with exactly the single word you printed.'\n"
    f"2. Use the Bash tool with run_in_background=true to run: sleep 4; echo BG-DONE > {BG}"
)
FOLLOWUP = ("Did the background bash task finish? If so, what single word did it "
            "write to the file?")


def main():
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, decision="allow")
    open(CAP, "w").close()
    try:
        os.remove(BG)
    except OSError:
        pass

    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()

    def both_done():
        ss = any(e.get("event") == "SubagentStop" for e in aoprobe.payloads())
        return os.path.exists(BG) and ss
    sess.run(until=both_done, max_s=160)
    sess._drain(2.0)

    # CRITICAL: drive a follow-up turn to flush the queued <task-notification>
    # onto the wire — without it the notification is never sent.
    time.sleep(1.0)
    sess.send(FOLLOWUP)
    time.sleep(0.4)
    sess.send("\r")
    deadline = time.time() + 40
    while time.time() < deadline:
        sess._pump_once(no_hook_yet=False)
        if any("task-notification" in json.dumps(b)
               for b in aoprobe.wire_request_bodies(CAP)):
            break
    sess._drain(3.0)
    sess.exit()

    rows = aoprobe.payloads()

    # --- subagent completion (HOOK path) ---
    sub_stop = [e for e in rows if e.get("event") == "SubagentStop"]
    sub_payload = sub_stop[0]["payload"] if sub_stop else {}
    sub_fields = {k: (str(sub_payload.get(k))[:80]) for k in
                  ("agent_id", "agent_type", "agent_transcript_path",
                   "last_assistant_message") if k in sub_payload}

    # --- bg bash completion (WIRE path, after follow-up) ---
    bodies = aoprobe.wire_request_bodies(CAP)
    notif = next((b for b in bodies if "task-notification" in json.dumps(b)), None)
    notif_text = ""
    if notif:
        s = json.dumps(notif)
        i = s.find("task-notification")
        notif_text = s[max(0, i - 30):i + 400]

    # --- dispatch-time 'running in background' result ---
    post_bash = [e for e in rows if e.get("event") == "PostToolUse" and e.get("tool") == "Bash"]
    dispatch = ""
    for e in post_bash:
        tr = json.dumps(e["payload"].get("tool_response", ""))
        if "background" in tr.lower() or '"task' in tr.lower():
            dispatch = tr[:300]
            break

    stop_fired = any(e.get("event") == "Stop" for e in rows)

    # Closeout (advisor #1): does the backgrounded Bash fire a hook at COMPLETION,
    # or only at DISPATCH? The earlier "no completion hook" read rested on an event
    # list that omitted PostToolUseFailure; with BOTH post-hooks now registered the
    # full timeline is authoritative. A genuine completion-time hook would be a Bash
    # post-hook whose TOOL_RESPONSE references the dispatch backgroundTaskId. Exactly
    # one such event is expected — the dispatch itself (empty stdout, "running in
    # background"). A SECOND reference would be a completion hook. We scope to
    # tool_response (NOT the whole payload) so the echoed command text ("echo
    # BG-DONE") and any follow-up file read don't false-positive.
    bg_task_id = ""
    m = re.search(r'"backgroundTaskId"\s*:\s*"([^"]+)"', dispatch)
    if m:
        bg_task_id = m.group(1)
    task_refs = [
        (e.get("event"), e.get("tool")) for e in rows
        if e.get("event") in ("PostToolUse", "PostToolUseFailure")
        and bg_task_id
        and bg_task_id in json.dumps(e["payload"].get("tool_response", ""))
    ]
    completion_hook = task_refs[1:]   # anything beyond the single dispatch event

    print("==== BACKGROUNDED SUBAGENT + BASH COMPLETION PROBE ====")
    print(f"bg.txt present (bg bash actually completed): {os.path.exists(BG)}")
    print("\n-- FULL hook timeline (every registered event, in order) --")
    for e in rows:
        print(f"   {str(e.get('event')):<22} tool={e.get('tool')}")
    print("\n-- SUBAGENT completion (SubagentStop HOOK) --")
    print(f"   SubagentStop fired: {bool(sub_stop)}")
    print(f"   carried fields: {json.dumps(sub_fields, indent=2)}")
    print("\n-- BG BASH completion (WIRE <task-notification>, after follow-up turn) --")
    print(f"   notification reached wire: {bool(notif)}")
    print(f"   notification ctx: {notif_text!r}")
    print("\n-- dispatch-time bg Bash tool_response --")
    print(f"   {dispatch!r}")
    print(f"\nStop (turn completion) hook fired: {stop_fired}")
    print(f"bg Bash dispatch backgroundTaskId: {bg_task_id!r}")
    print(f"Bash post-hooks referencing that task id in tool_response: {task_refs}  (1 = dispatch only)")
    print(f"hook firing at bg-Bash COMPLETION (expect [] — completion is wire-only): {completion_hook}")
    ok = (bool(sub_stop) and bool(sub_payload.get("last_assistant_message"))
          and bool(notif) and not completion_hook)
    print(f"\nVERDICT: {'CONFIRMED — distinct completion signals: subagent via hook, bg bash via wire (no completion hook)' if ok else 'PARTIAL — inspect above'}")
    print("pty log:", PTY_LOG)
    print("=======================================================")


if __name__ == "__main__":
    main()
