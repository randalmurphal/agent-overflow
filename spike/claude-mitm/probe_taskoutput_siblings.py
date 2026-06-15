#!/usr/bin/env python3
"""Probe: EXACT /v1/messages wire shape when a backgrounded sibling completes
WHILE a TaskOutput(block=true) call is waiting on a DIFFERENT background task.

Reproduces the user's claude-tui bug scenario on the real binary:
  1. Bash run_in_background=true  -> 5s ticker
  2. Bash run_in_background=true  -> 10s ticker
  3. TaskOutput block=true on the 10s task  (the 5s sibling finishes mid-wait)
  4. stop

The real TUI transcript shows BOTH commands then emit a <task-notification>
(the waited one AND the sibling), flushed as queued_command attachments. This
probe captures the actual /v1/messages REQUEST BODY to answer the one question
the transcript can't: are those notifications

  (a) SEPARATE user messages (one <task-notification> each), or
  (b) BUNDLED into one user message (multiple <task-notification> blocks), or
  (c) folded into the SAME user message as the TaskOutput tool_result?

That packing decides whether claudetui's first-notification-only extraction
(ExtractTaskNotificationFields) silently drops the sibling.

Self-contained: starts its OWN aocap proxy on a random loopback port (no
conflict with a running Agent Overflow app), drives claude through it, then
dumps the trailing message structure of every captured /v1/messages request.
"""
import json
import os
import subprocess
import sys
import time

import aoprobe

HERE = os.path.dirname(os.path.abspath(__file__))
AOCAP_SRC = os.path.join(HERE, "aocap", "main.go")
CAP = "/tmp/ao-taskoutput-cap.jsonl"
PTY_LOG = f"{aoprobe.AOHOOK}/pty-taskoutput.log"

PROMPT = (
    "Do exactly these four steps using your tools, then stop. Do not ask for "
    "confirmation.\n"
    "1. Use the Bash tool with run_in_background=true to run this 5-second "
    "command: for i in 1 2 3 4 5; do echo \"5s-tick $i\"; sleep 1; done; echo '5s done'\n"
    "2. Use the Bash tool with run_in_background=true to run this 10-second "
    "command: for i in 1 2 3 4 5 6 7 8 9 10; do echo \"10s-tick $i\"; sleep 1; done; echo '10s done'\n"
    "3. Immediately use the TaskOutput tool with block=true to wait for the "
    "10-second command to finish and retrieve its output.\n"
    "4. Report both commands' final output in one short message, then stop."
)


def start_proxy():
    """Build + run aocap on a random port; return (proc, base_url)."""
    binp = "/tmp/aocap_bin"
    build = subprocess.run(["go", "build", "-o", binp, AOCAP_SRC],
                           capture_output=True, text=True)
    if build.returncode != 0:
        sys.exit(f"aocap build failed:\n{build.stderr}")
    proc = subprocess.Popen(
        [binp, "-listen", "127.0.0.1:0", "-cap", CAP],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    # First stdout line is {"listen": "127.0.0.1:PORT"} once bound.
    line = proc.stdout.readline()
    try:
        addr = json.loads(line)["listen"]
    except (json.JSONDecodeError, KeyError):
        proc.kill()
        sys.exit(f"proxy did not report listen addr; got: {line!r}")
    return proc, f"http://{addr}"


def block_text(content):
    """Mirror claudetui classify.go blockText: top-level {type:text} only."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(b.get("text", "") for b in content
                         if isinstance(b, dict) and b.get("type") == "text")
    return ""


def describe_block(b):
    if not isinstance(b, dict):
        return {"t": "RAW", "v": str(b)[:60]}
    t = b.get("type")
    if t == "text":
        txt = b.get("text", "")
        return {"t": t, "n_notif": txt.count("<task-notification"),
                "head": txt[:80].replace("\n", "\\n")}
    if t == "tool_result":
        c = b.get("content", "")
        cs = c if isinstance(c, str) else json.dumps(c)
        return {"t": t, "tuid": b.get("tool_use_id"),
                "n_notif": cs.count("<task-notification"),
                "head": cs[:80].replace("\n", "\\n")}
    if t == "tool_use":
        return {"t": t, "name": b.get("name"), "id": b.get("id"),
                "input": json.dumps(b.get("input", {}))[:80]}
    return {"t": t}


def main():
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, decision="allow")
    open(CAP, "w").close()
    proxy, base_url = start_proxy()
    print(f"[probe] proxy at {base_url}, cap -> {CAP}", file=sys.stderr)

    sess = aoprobe.ClaudeSession(PROMPT, base_url, PTY_LOG)
    sess.start()

    def turn_done():
        return any(e.get("event") == "Stop" for e in aoprobe.payloads())

    ok = sess.run(until=turn_done, max_s=120)
    sess._drain(3.0)
    sess.exit()
    time.sleep(0.5)
    proxy.terminate()
    try:
        proxy.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proxy.kill()

    print(f"\n==== TASKOUTPUT-SIBLING WIRE PROBE  (Stop fired: {ok}) ====\n")
    bodies = []
    for r in aoprobe.wire_records(CAP):
        if r.get("kind") == "request" and r.get("path") == "/v1/messages" and r.get("body"):
            try:
                bodies.append((r.get("req_id"), json.loads(r["body"])))
            except (json.JSONDecodeError, ValueError):
                continue
    print(f"/v1/messages requests captured: {len(bodies)}\n")

    for rid, body in bodies:
        msgs = body.get("messages", [])
        # Does this request carry a <task-notification> anywhere in messages?
        raw = json.dumps(msgs)
        has = "<task-notification" in raw
        if not has:
            continue
        total = raw.count("<task-notification")
        print(f"--- request req_id={rid}: {len(msgs)} messages, "
              f"{total} <task-notification> total ---")
        # Show the trailing 6 messages with full block structure.
        for i, m in enumerate(msgs[-6:]):
            idx = len(msgs) - 6 + i
            role = m.get("role")
            content = m.get("content")
            if isinstance(content, list):
                blocks = [describe_block(b) for b in content]
            else:
                s = str(content)
                blocks = [{"t": "STR", "n_notif": s.count("<task-notification"),
                           "head": s[:80].replace("\n", "\\n")}]
            # What claudetui's eachTaskNotification would extract: ONE call to
            # ExtractTaskNotificationFields(blockText(content)). Count notifs in
            # blockText (top-level text only) vs total in the message.
            bt = block_text(content)
            print(f"  msg[{idx}] role={role}")
            for b in blocks:
                print(f"      {json.dumps(b)}")
            print(f"      blockText() notif_count={bt.count('<task-notification')} "
                  f"(claudetui sees these; extracts only the FIRST)")
        print()

    # Persist a decoded copy for offline inspection.
    with open("/tmp/ao-taskoutput-decoded.json", "w") as f:
        json.dump([{"req_id": rid, "messages": b.get("messages", [])}
                   for rid, b in bodies], f, indent=2)
    print("decoded bodies -> /tmp/ao-taskoutput-decoded.json")
    print("pty log ->", PTY_LOG)


if __name__ == "__main__":
    main()
