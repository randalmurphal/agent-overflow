#!/usr/bin/env python3
"""Probe: interrupt DURING assistant TEXT STREAMING (not tool execution). Does
the partial text that already streamed survive — and where does AO recover it?

Open item #2. The earlier streaming-interrupt artifact fired BEFORE any text
delta, so it never tested persistence of text that HAD streamed.

Framing (per review): the PRIMARY recovery is the WIRE. The text deltas reached
the proxy live, before the Esc, so AO has the partial text by construction — the
pass condition for "AO can show partial text" is the captured deltas. The
transcript is a SEPARATE question (does an interrupted partial assistant turn
persist on disk for crash recovery?); a transcript drop is a crash-recovery
limitation, NOT a display gap.

Method: prompt a long answer; truncate the shared cap log; pump until the wire
shows real `text_delta`s (the answer is mid-stream, not just thinking); send one
Esc; then check the wire turn for deltas + absence of `message_stop` (aborted),
and whether the partial text persisted to the transcript.
"""
import json
import os
import re
import time

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
CAP = os.environ.get("AO_CAP_LOG", "/tmp/ao-cap.jsonl")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-partialtext.log"
PROMPT = ("Write a detailed ~400 word explanation of how TCP congestion control "
          "works — cover slow start, congestion avoidance, fast retransmit, and "
          "fast recovery. Begin the explanation immediately, no preamble.")

_DELTA = re.compile(r'"text_delta","text":("(?:[^"\\]|\\.)*")')


def streaming_req():
    """req_id of a turn that is mid-text-stream (>=2 text_delta, not yet ended)."""
    for rid, slot in aoprobe.wire_sse_by_req(CAP).items():
        if slot["text"].count("text_delta") >= 2 and not slot["ended"]:
            return rid
    return None


def main():
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, decision="allow")
    open(CAP, "w").close()             # fresh wire view for this probe
    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()

    esc_at = None
    target = None
    while sess.elapsed() < 60:
        sess._pump_once(no_hook_yet=(not aoprobe.payloads()))
        rid = streaming_req()
        if rid and esc_at is None:
            target = rid
            time.sleep(0.4)            # let a little more text stream
            sess.send("\x1b")          # Esc mid-stream
            esc_at = time.time()
            print(f"[probe] Esc at {sess.elapsed():.1f}s, mid-stream req={rid}")
        if esc_at and time.time() - esc_at > 4:
            break
    sess._drain(3.0)
    sess.exit()

    by = aoprobe.wire_sse_by_req(CAP)
    sse = by.get(target, {}).get("text", "") if target else ""
    wire_streamed = sse.count("text_delta") >= 2
    wire_no_stop = "message_stop" not in sse

    streamed = ""
    for mt in _DELTA.finditer(sse):
        try:
            streamed += json.loads(mt.group(1))
        except (json.JSONDecodeError, ValueError):
            pass
    frag = streamed.strip()[:40]

    rows = aoprobe.payloads()
    tpath = next((e["payload"].get("transcript_path") for e in rows
                  if e["payload"].get("transcript_path")), None)
    transcript_has_partial = False
    if frag and tpath and os.path.exists(tpath):
        transcript_has_partial = frag in open(tpath, errors="replace").read()

    print("==== PARTIAL-TEXT INTERRUPT PROBE ====")
    print(f"target streaming req: {target}")
    print(f"[WIRE / primary] text streamed before Esc: {wire_streamed}")
    print(f"[WIRE / primary] no message_stop (aborted mid-stream): {wire_no_stop}")
    print(f"reconstructed streamed prefix: {frag!r}")
    print(f"[TRANSCRIPT / crash-recovery] partial persisted on disk: {transcript_has_partial}")
    primary_ok = wire_streamed and wire_no_stop
    print(f"\nPRIMARY (AO can display partial text from wire): "
          f"{'CONFIRMED' if primary_ok else 'NOT — too late or no stream'}")
    print(f"BONUS (transcript crash-recovery of partial): "
          f"{'persists' if transcript_has_partial else 'dropped (display unaffected)'}")
    print("pty log:", PTY_LOG)
    print("======================================")


if __name__ == "__main__":
    main()
