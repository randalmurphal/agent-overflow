#!/usr/bin/env python3
"""Probe B: mid-turn steering capture.

User's crux: mid-turn steering "works as-is, as long as we can properly capture
when the message is queued -> the chat history." So the question is purely about
the OBSERVABLE capture point: when the user submits a second message while a turn
is running, WHEN and WHERE does AO see it?

  hook       does UserPromptSubmit fire at SUBMIT (the moment it's queued) or only
             at PICKUP (when the turn consumes it)? -> the capture point.
  consumed   is the steering message acted on in the SAME turn (true steering) or
             deferred to a NEW turn after Stop? (count Stops before it's consumed)
  wire       which request's messages[] first carries the steering text — a
             continuation of the running turn, or a fresh request after Stop?
  transcript when does the steering user row land?
  PTY        does the TUI show a "queued" affordance?

Method: launch a multi-step Bash turn (decision=allow, so steps run without
prompts), wait for the FIRST PreToolUse (turn is definitely running), then inject
the steering message + Enter. The steering asks the agent to `echo <STEERED>` as
its next command; if that token shows up in a later Bash tool event, the steering
was consumed. Driving here (typing the steering message) is the POINT — it's
exactly what AO relays for the user, so keystrokes>0 is expected.

Markers are unique per run (the shared proxy cap file accumulates across probes;
a reused marker would match a prior run — the bug that bit probe A).
"""
import os
import time
import json
import uuid

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
CAP = os.environ.get("AO_CAP", "/tmp/ao-cap-danger.jsonl")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-steer.log"

SESSION_ID = str(uuid.uuid4())
PROJ = aoprobe.CWD.replace("/", "-")
TRANSCRIPT = f"/tmp/aoclaude/projects/{PROJ}/{SESSION_ID}.jsonl"

TAG = SESSION_ID.split("-")[0].upper()
MA = "STEERINIT" + TAG       # initial multi-step prompt marker
MB = "STEERMSG" + TAG        # the steering message marker
STEERED = "STEERDONE" + TAG  # token the steering tells the agent to echo

PROMPT = (
    f"{MA}. Use the Bash tool to run these as five SEPARATE commands, one tool "
    f"call at a time, and after each one write a short sentence about it: echo "
    f"{MA}1 ; then echo {MA}2 ; then echo {MA}3 ; then echo {MA}4 ; then echo "
    f"{MA}5. Run each as its own Bash call. After all five, reply with READY."
)
STEER = (
    f"{MB}. Change of plan: as your very next Bash command, run exactly this: "
    f"echo {STEERED}"
)


def read_rows(path):
    if not os.path.exists(path):
        return []
    out = []
    for line in open(path):
        line = line.strip()
        if not line:
            continue
        try:
            out.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return out


def _blob(d):
    return json.dumps(d or {})


def user_rows_with(rows, marker):
    return [r for r in rows if r.get("type") == "user"
            and marker in (lambda c: c if isinstance(c, str) else json.dumps(c))(
                (r.get("message") or {}).get("content"))]


def req_bodies_in_order(cap):
    """(req_id, raw_body_str) for every captured request, in capture order."""
    return [(r.get("req_id"), r.get("body") or "")
            for r in aoprobe.wire_records(cap) if r.get("kind") == "request"]


def main():
    aoprobe.seed_config(
        events=["UserPromptSubmit", "Stop", "PreToolUse", "PostToolUse"],
        decision="allow")
    try:
        os.remove(TRANSCRIPT)
    except OSError:
        pass

    sess = aoprobe.ClaudeSession(
        PROMPT, BASE_URL, PTY_LOG, extra_args=["--session-id", SESSION_ID])
    sess.start()

    injected = submitted = False
    inject_at = submit_at = None
    inject_text_t = None
    queued_marker_seen = False
    pre_count_at_inject = 0

    while sess.elapsed() < 160:
        sess._pump_once(no_hook_yet=(not aoprobe.payloads()))
        rows = aoprobe.payloads()
        pre = [e for e in rows if e.get("event") == "PreToolUse"]

        # type the steering message the instant the turn is provably running
        if not injected and pre:
            pre_count_at_inject = len(pre)
            sess.send(STEER)
            inject_at = sess.elapsed()
            inject_text_t = time.time()
            injected = True

        # submit it only AFTER the composer has rendered the burst — a \r sent
        # in the same instant as an ~80-char paste gets coalesced/dropped (proven
        # in run 1: the text landed in the composer but never queued).
        if injected and not submitted and time.time() - inject_text_t > 1.3:
            sess.send("\r")
            submit_at = sess.elapsed()
            submitted = True

        if submitted:
            norm = aoprobe._norm(sess._rawtail)
            if b"queued" in norm or b"willbesent" in norm or b"queue" in norm:
                queued_marker_seen = True
            consumed = any(STEERED in _blob(e.get("payload")) for e in rows)
            stops = [e for e in rows if e.get("event") == "Stop"]
            # done when steering is consumed, or it's clearly been given a full
            # extra turn after submit without being consumed
            if consumed:
                break
            if len(stops) >= 2 and sess.elapsed() - submit_at > 15:
                break
    sess._drain(2.5)

    rows = aoprobe.payloads()
    pty_tail = aoprobe._norm(sess._rawtail)
    if b"queued" in pty_tail or b"willbesent" in pty_tail or b"queue" in pty_tail:
        queued_marker_seen = True
    keystrokes = sess.keystrokes
    sess.exit()

    # ---- hook timeline analysis ----
    ups = [e for e in rows if e.get("event") == "UserPromptSubmit"]
    ups_mb_idx = next((i for i, e in enumerate(rows)
                       if e.get("event") == "UserPromptSubmit"
                       and MB in _blob(e.get("payload"))), None)
    # how many tool events / Stops occurred BEFORE UPS(MB) landed in the log
    pre_before_ups = stops_before_ups = 0
    if ups_mb_idx is not None:
        pre_before_ups = sum(1 for e in rows[:ups_mb_idx]
                             if e.get("event") == "PreToolUse")
        stops_before_ups = sum(1 for e in rows[:ups_mb_idx]
                               if e.get("event") == "Stop")

    # consumption: STEERED echo shows up in a Bash tool event, and how many Stops first
    consume_idx = next((i for i, e in enumerate(rows)
                        if STEERED in _blob(e.get("payload"))), None)
    consumed = consume_idx is not None
    stops_before_consume = (sum(1 for e in rows[:consume_idx]
                                if e.get("event") == "Stop")
                            if consumed else None)
    total_stops = sum(1 for e in rows if e.get("event") == "Stop")

    # ---- wire analysis: which request first carries the steering text ----
    bodies = req_bodies_in_order(CAP)
    mb_req_index = next((i for i, (_, b) in enumerate(bodies) if MB in b), None)
    # of THIS run's requests only (those carrying MA or MB), where does MB first appear?
    run_bodies = [b for (_, b) in bodies if MA in b or MB in b]
    mb_run_index = next((i for i, b in enumerate(run_bodies) if MB in b), None)

    # ---- transcript ----
    # Steering is NOT stored as a type=user row. It lands as a `queue-operation`
    # (operation:enqueue, content, timestamp) plus an `attachment` of type
    # `queued_command` — those are the durable capture points, not user rows.
    tx = read_rows(TRANSCRIPT)
    mb_user_rows = len(user_rows_with(tx, MB))
    ma_user_rows = len(user_rows_with(tx, MA))
    mb_queue_ops = [r for r in tx if r.get("type") == "queue-operation"
                    and MB in json.dumps(r)]
    mb_queued_attach = [r for r in tx if r.get("type") == "attachment"
                        and (r.get("attachment") or {}).get("type") == "queued_command"
                        and MB in json.dumps(r)]

    print("==== MID-TURN STEERING CAPTURE ====")
    print(f"session-id: {SESSION_ID}")
    print(f"target: 2.1.158 binary   injected={injected} at t={inject_at}s "
          f"(after {pre_count_at_inject} PreToolUse)   keystrokes={keystrokes}")

    print("\n-- HOOK (the capture point) --")
    print(f"   UserPromptSubmit(steering MB) fired: {ups_mb_idx is not None}  "
          f"(total UPS={len(ups)})")
    print(f"   PreToolUse events before UPS(MB) landed: {pre_before_ups}   "
          f"Stops before UPS(MB): {stops_before_ups}")
    if ups_mb_idx is None:
        print("   => UPS(MB) NEVER fired — a queued steer is NOT surfaced on the hook channel")
    elif stops_before_ups == 0:
        print("   => fired ~immediately at submit (the queue capture point)")
    else:
        print("   => UPS(MB) deferred past a Stop (pickup-time, not submit-time)")

    print("\n-- CONSUMPTION (steering vs deferred) --")
    print(f"   steering consumed (agent echoed {STEERED}): {consumed}")
    print(f"   Stops before consumption: {stops_before_consume}   total Stops: {total_stops}")
    if consumed and stops_before_consume == 0:
        print("   => SAME-TURN steering: the running turn picked up the message")
    elif consumed:
        print("   => DEFERRED: consumed only after the turn ended (new turn)")
    else:
        print("   => NOT consumed within the window (inspect pty log)")

    print("\n-- WIRE --")
    print(f"   steering text first appears in captured request #{mb_req_index} (all reqs), "
          f"#{mb_run_index} (this run's reqs)")

    print("\n-- TRANSCRIPT --")
    print(f"   initial-prompt user rows: {ma_user_rows}   steering as type=user rows: {mb_user_rows}")
    print(f"   steering enqueue rows (type=queue-operation): {len(mb_queue_ops)}   "
          f"queued_command attachments: {len(mb_queued_attach)}")
    if mb_queue_ops:
        op = mb_queue_ops[0]
        print(f"      -> operation={op.get('operation')!r}  ts={op.get('timestamp')}  "
              f"content={op.get('content','')[:50]!r}")

    print("\n-- PTY (TUI scrape) --")
    print(f"   'queued' affordance seen in TUI: {queued_marker_seen}")
    print("transcript:", TRANSCRIPT)
    print("pty log:", PTY_LOG)
    print("===================================")


if __name__ == "__main__":
    main()
