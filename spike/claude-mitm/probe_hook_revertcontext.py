#!/usr/bin/env python3
"""Probe A2: after a think-only Esc-revert, does the reverted prompt RE-ENTER the
next turn's context, or is it dropped?

probe_hook_escrevert.py established that a think-only Esc aborts the in-flight
/v1/messages (no message_stop, upstream_read "context canceled") AND leaves an
orphaned `user` row in the transcript (present->present). The open question that
flips AO's reconciliation: when the user submits the NEXT prompt in the same
live process, does Claude Code's in-memory conversation still contain the
reverted prompt (=> it was only "unsent", AO shows it pending) or has it dropped
it (=> AO treats the orphan as reverted and must filter the stale transcript row
on backfill)?

The wire is the source of truth for "what context the model sees". So:
  1. fresh session, think-only prompt A (marker MA) at --effort max
  2. Esc at the first thinking chunk on the wire  -> revert
  3. confirm the revert regime from A's SSE (thinking, no output, no message_stop)
  4. clear the composer (Ctrl-E to end + a run of backspaces) and submit a short
     prompt B (marker MB)
  5. capture B's request body and walk messages[]:
        a message with MA but NOT MB  -> A re-enters as a distinct history turn
                                          => STILL PENDING
        MA absent entirely            -> A dropped from live context => DROPPED
        MA only ever co-located with MB in one message -> composer leftover fused
                                          => inconclusive (clear failed); rerun

Driving here is intentional and minimal: the trust \r, one Esc (revert), a
composer clear, prompt B + \r, teardown esc. The structural messages[] analysis
is robust to an imperfect composer clear (the fused case is detected, not
silently miscounted as a re-entry).
"""
import os
import time
import json
import uuid

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
CAP = os.environ.get("AO_CAP", "/tmp/ao-cap-danger.jsonl")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-revertcontext.log"

SESSION_ID = str(uuid.uuid4())
PROJ = aoprobe.CWD.replace("/", "-")
TRANSCRIPT = f"/tmp/aoclaude/projects/{PROJ}/{SESSION_ID}.jsonl"

TAG = SESSION_ID.split("-")[0].upper()
MA = "REVERTCTXA" + TAG      # the reverted prompt's marker
MB = "REVERTCTXB" + TAG      # the follow-up prompt's marker

PROMPT_A = (
    f"{MA}. Think silently and very thoroughly in your reasoning before writing "
    "anything, and do not use any tools. Enumerate, one at a time in your "
    "reasoning, every ordered triple (a, b, c) of positive integers with "
    "a < b < c and a + b + c = 20, verifying each candidate before moving on, "
    "then count them and state the total."
)
PROMPT_B = f"{MB}. Reply with exactly the single word OKAY and nothing else."


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


def req_ids_with_marker(cap, marker):
    return [r.get("req_id") for r in aoprobe.wire_records(cap)
            if r.get("kind") == "request" and marker in (r.get("body") or "")]


def body_for_req(cap, req_id):
    for r in aoprobe.wire_records(cap):
        if r.get("kind") == "request" and r.get("req_id") == req_id:
            return r.get("body") or ""
    return ""


def _msg_text(m):
    """Flatten a /v1/messages message's content (str or block list) to text."""
    c = m.get("content")
    if isinstance(c, str):
        return c
    if isinstance(c, list):
        parts = []
        for b in c:
            if isinstance(b, dict):
                parts.append(b.get("text") or b.get("content") or json.dumps(b))
            else:
                parts.append(str(b))
        return " ".join(parts)
    return json.dumps(c)


def main():
    aoprobe.seed_config(
        events=["UserPromptSubmit", "Stop", "PreToolUse", "PostToolUse"],
        decision="allow")
    try:
        os.remove(TRANSCRIPT)
    except OSError:
        pass

    sess = aoprobe.ClaudeSession(
        PROMPT_A, BASE_URL, PTY_LOG,
        extra_args=["--session-id", SESSION_ID, "--effort", "max"])
    sess.start()

    # 1+2) Esc at the first thinking chunk on the wire (reliably pre-output).
    esc_done = False
    a_rid = None
    while sess.elapsed() < 70 and not esc_done:
        sess._pump_once(no_hook_yet=(not aoprobe.payloads()))
        rids = req_ids_with_marker(CAP, MA)
        if rids:
            a_rid = rids[-1]
            txt = aoprobe.wire_sse_by_req(CAP).get(a_rid, {}).get("text", "")
            if "thinking" in txt.lower():
                sess.send("\x1b")           # revert
                esc_done = True
    if not esc_done and a_rid is None:
        print("ABORT: A's request never seen on the wire — inspect proxy/cap.")
        sess.exit()
        return

    # 3) settle, then check the regime from A's SSE
    settle = time.time()
    while time.time() - settle < 5.0:
        sess._pump_once(no_hook_yet=False)

    a_sse = aoprobe.wire_sse_by_req(CAP).get(a_rid, {})
    a_text = a_sse.get("text", "")
    streamed_thinking = "thinking" in a_text.lower()
    streamed_output = any(m in a_text for m in
                          ('"text_delta"', '"input_json_delta"', '"tool_use"',
                           '"output_text"'))
    a_has_stop = "message_stop" in a_text
    composer_after_revert = MA.lower() in aoprobe._norm(sess._rawtail).decode(
        "ascii", "replace")

    if streamed_output or not streamed_thinking:
        print("==== REVERT-CONTEXT (think-only re-entry) ====")
        print(f"session-id: {SESSION_ID}")
        print(f"REGIME NOT think-only (thinking={streamed_thinking} "
              f"output={streamed_output}) — cannot test revert re-entry; rerun.")
        sess.exit()
        return

    # 4) clear the composer (best effort) and submit B
    sess.send("\x05")                       # Ctrl-E: cursor to end
    for _ in range(420):                    # backspace the restored prompt away
        sess.send("\x7f")
    drain = time.time()
    while time.time() - drain < 1.5:
        sess._pump_once(no_hook_yet=False)
    composer_cleared = MA.lower() not in aoprobe._norm(
        sess._rawtail).decode("ascii", "replace")

    sess.send(PROMPT_B)
    btext_t = time.time()
    submitted = False
    b_rid = None
    while sess.elapsed() < 150:
        sess._pump_once(no_hook_yet=False)
        if not submitted and time.time() - btext_t > 1.3:
            sess.send("\r")                 # submit after the paste renders
            submitted = True
        if submitted:
            rids = req_ids_with_marker(CAP, MB)
            if rids:
                b_rid = rids[-1]
                # give the body a beat to be fully captured
                sess._drain(1.0)
                break
    sess._drain(2.0)

    keystrokes = sess.keystrokes
    sess.exit()

    # 5) structural analysis of B's request messages[]
    b_body = body_for_req(CAP, b_rid) if b_rid else ""
    msgs = []
    try:
        msgs = (json.loads(b_body) or {}).get("messages", []) if b_body else []
    except (json.JSONDecodeError, ValueError):
        msgs = []

    roles = []
    a_only = b_only = both = 0
    for m in msgs:
        t = _msg_text(m)
        ha, hb = MA in t, MB in t
        roles.append((m.get("role"), "A" if ha else "", "B" if hb else ""))
        if ha and hb:
            both += 1
        elif ha:
            a_only += 1
        elif hb:
            b_only += 1

    tx = read_rows(TRANSCRIPT)
    ma_user_rows = sum(1 for r in tx if r.get("type") == "user"
                       and MA in _msg_text(r.get("message") or {}))

    print("==== REVERT-CONTEXT: does a reverted prompt re-enter the next turn? ====")
    print(f"session-id: {SESSION_ID}")
    print(f"target: 2.1.158 binary | effort=max | keystrokes(drive): {keystrokes}")
    print(f"\nREGIME: THINK-ONLY confirmed "
          f"(thinking={streamed_thinking} output={streamed_output} "
          f"message_stop={a_has_stop})")
    print(f"   A req_id={a_rid}  composer held A after revert={composer_after_revert}")
    print(f"   orphaned A user-row in transcript: {ma_user_rows}")

    print(f"\n-- composer clear before B: {'OK' if composer_cleared else 'INCOMPLETE'}")
    print(f"-- B req_id={b_rid}  messages[] count={len(msgs)}")
    print(f"   messages with A-only={a_only}  B-only={b_only}  both(A+B)={both}")
    for i, (role, a, b) in enumerate(roles):
        print(f"      [{i}] role={role:9} {('MA' if a else '  ')} {('MB' if b else '  ')}")

    print("\nVERDICT:")
    if not b_rid:
        print("  INCONCLUSIVE — B's request never captured. Inspect pty/cap.")
    elif a_only >= 1:
        print("  RE-ENTERS: the reverted prompt is a DISTINCT user message in B's")
        print("  request context. The revert only 'unsent' it; it is STILL PENDING.")
        print("  => AO should keep the message as pending, not drop it on revert.")
    elif both >= 1 and a_only == 0:
        print("  INCONCLUSIVE (FUSED): A appears only co-located with B in one")
        print("  message — composer was not fully cleared, so A rode along as typed")
        print("  text, not as replayed history. Rerun with a cleaner composer clear.")
    elif a_only == 0 and both == 0:
        print("  DROPPED: the reverted prompt is ABSENT from B's request context.")
        print("  Claude Code drops it from the live conversation on revert.")
        print("  => AO treats the orphan as reverted; the durable transcript still")
        print("     holds a stale user row that AO must filter on backfill/resume.")
    else:
        print("  Mixed — see the messages[] table above.")
    print("transcript:", TRANSCRIPT)
    print("pty log:", PTY_LOG)
    print("=======================================================================")


if __name__ == "__main__":
    main()
