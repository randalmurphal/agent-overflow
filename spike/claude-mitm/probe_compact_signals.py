#!/usr/bin/env python3
"""Validate a SIGNAL-BASED (not timing-based) compaction detector for claudetui.

The summarizer /v1/messages request is wire-identical to a normal turn
(diff_summarizer.py: same system, metadata, headers, tools; only max_tokens +
injected content differ). So detection must come from the hook lifecycle AROUND
the request. The proposed positive-signal state machine:

    PreCompact        -> ARM  (a compaction attempt began)
    UserPromptSubmit  -> DISARM (a real user turn is next; the summarizer fork
                                 is NOT preceded by a user prompt)
    agent POST while ARMED -> that POST is the SUMMARIZER (capture, don't render)
    PostCompact       -> SUCCESS finalize (carries compact_summary)
    summarizer SSE error / Notification(error-compacting) -> FAILURE finalize

This probe prints the exact ORDERED interleaving of hook events and /v1/messages
POSTs for two scenarios so we can confirm the machine is sound:

  A) REJECTED /compact (too few messages) THEN a real follow-up turn.
     MUST show: PreCompact ... UserPromptSubmit(real) ... POST(real)
     i.e. a UserPromptSubmit disarms BEFORE the real turn's POST -> the real
     turn is never mistaken for the summarizer.

  B) SUCCESSFUL /compact THEN a real follow-up turn.
     MUST show: [maybe UserPromptSubmit(/compact)] PreCompact -> POST(summarizer)
     with NO UserPromptSubmit between PreCompact and the summarizer POST -> the
     summarizer is unambiguously "the armed POST".

Run:
  PT=$(mktemp -d)
  go build -C proxy -o "$PT/ao-proxy" .
  "$PT/ao-proxy" --listen 127.0.0.1:8098 --log "$PT/cap.jsonl" &
  AO_BASE_URL=http://127.0.0.1:8098 AO_CAP_LOG="$PT/cap.jsonl" python3 probe_compact_signals.py
"""
import json
import os
import time

import aoprobe
from probe_compact import req_view, is_agent, agent_end_turns, n_posts, \
    send_line, wait_for_proxy

MARKER = "Your task is to create a detailed summary of"
PROMPTS = [
    "In one sentence, what is a hash table?",
    "In one sentence, what is a binary search tree?",
    "In one sentence, what is a linked list?",
    "In one sentence, what is a stack?",
    "In one sentence, what is a queue?",
    "In one sentence, what is a heap?",
    "In one sentence, what is a graph?",
    "In one sentence, what is a trie?",
]
PTY_LOG = "/tmp/aocompact/compact-signals-pty.log"


def parse_proxy_ts(s):
    if not s:
        return None
    s = s.replace("Z", "+00:00")
    if "." in s:
        head, frac = s.split(".", 1)
        tz = ""
        for sign in ("+", "-"):
            if sign in frac:
                frac, t = frac.split(sign, 1)
                tz = sign + t
                break
        s = f"{head}.{frac[:6]}{tz}"
    from datetime import datetime
    try:
        return datetime.fromisoformat(s).timestamp()
    except ValueError:
        return None


def classify_post(body_raw):
    try:
        b = json.loads(body_raw or "{}")
    except (json.JSONDecodeError, ValueError):
        return "unparseable", {}
    if MARKER in (body_raw or ""):
        return "SUMMARIZER", b
    if (b.get("max_tokens") or 0) <= 1:
        return "quota", b
    if len(b.get("tools", [])) == 0:
        return "aux", b
    return "agent", b


def unified_timeline(cap, t0):
    """Merge hook payloads + /v1/messages POST requests into one ts-ordered list."""
    events = []
    for p in aoprobe.payloads():
        ev = p.get("event")
        if ev in ("UserPromptSubmit", "PreCompact", "PostCompact", "Notification",
                  "SessionStart", "Stop"):
            detail = ""
            pl = p.get("payload", {})
            if ev == "UserPromptSubmit":
                detail = (pl.get("prompt") or "")[:40].replace("\n", " ")
            elif ev == "PostCompact":
                detail = f"summary_len={len(pl.get('compact_summary') or '')}"
            elif ev == "PreCompact":
                detail = f"trigger={pl.get('trigger')}"
            elif ev == "Notification":
                detail = (pl.get("message") or pl.get("text") or "")[:50]
            events.append((p.get("ts", 0), "HOOK", ev, detail))
    for r in aoprobe.wire_records(cap):
        if r.get("kind") == "request" and r.get("path") == "/v1/messages":
            ts = parse_proxy_ts(r.get("ts"))
            kind, b = classify_post(r.get("body"))
            detail = f"max_tok={b.get('max_tokens')} n_tools={len(b.get('tools', []))}"
            events.append((ts or 0, "WIRE", f"POST:{kind}", detail))
    events.sort(key=lambda e: e[0])
    return [(round(ts - t0, 3), src, ev, detail) for ts, src, ev, detail in events]


def main():
    cap = os.environ["AO_CAP_LOG"]
    base = os.environ["AO_BASE_URL"]
    wait_for_proxy(base)
    os.makedirs("/tmp/aocompact", exist_ok=True)

    aoprobe.seed_config(["PreCompact", "PostCompact", "UserPromptSubmit",
                         "Notification", "SessionStart", "Stop"])

    sess = aoprobe.ClaudeSession(
        PROMPTS[0], base, PTY_LOG, extra_args=["--model", "haiku"])
    sess.start()
    t0 = time.time()
    print("[A] warm 1 turn ...", flush=True)
    sess.run(until=lambda: len(agent_end_turns(req_view(cap))) >= 1, max_s=180)

    # --- A: rejected /compact, THEN a real follow-up turn ---
    posts0 = n_posts(cap)
    print("[A] /compact (expect reject) ...", flush=True)
    send_line(sess, "/compact")
    sess.run(until=lambda: n_posts(cap) > posts0
             and (time.time() - sess._last_out) > 2.5, max_s=20)
    time.sleep(1.0)
    turns_before_follow = len(agent_end_turns(req_view(cap)))
    print("[A] real follow-up turn after rejected /compact ...", flush=True)
    send_line(sess, "In one sentence, what is recursion?")
    sess.run(until=lambda: len(agent_end_turns(req_view(cap))) > turns_before_follow,
             max_s=200)
    print(f"[A] done. agent_turns={len(agent_end_turns(req_view(cap)))}", flush=True)

    # --- B: build context, successful /compact, then a real follow-up ---
    print(f"[B] building context to clear the floor ...", flush=True)
    for k, p in enumerate(PROMPTS, start=1):
        if k > 1:
            send_line(sess, p)
            sess.run(until=lambda k=k: len(agent_end_turns(req_view(cap))) >= k,
                     max_s=200)
    posts1 = n_posts(cap)
    print(f"[B] /compact (expect success, posts={posts1}) ...", flush=True)
    send_line(sess, "/compact")
    sess.run(until=lambda: n_posts(cap) > posts1
             and (time.time() - sess._last_out) > 2.5, max_s=360)
    time.sleep(1.5)
    tb = len(agent_end_turns(req_view(cap)))
    print(f"[B] real follow-up turn after successful /compact ...", flush=True)
    send_line(sess, "Reply with only the single word READY.")
    sess.run(until=lambda: len(agent_end_turns(req_view(cap))) > tb, max_s=200)
    print(f"[B] done.", flush=True)

    time.sleep(1.0)
    sess.exit()

    # ---------------- TIMELINE ----------------
    print("\n" + "=" * 78)
    print("UNIFIED TIMELINE (hooks + /v1/messages POSTs), t relative to session start")
    print("=" * 78)
    tl = unified_timeline(cap, t0)
    for t, src, ev, detail in tl:
        tag = "  HOOK" if src == "HOOK" else "WIRE  "
        print(f"  [{t:8.3f}] {tag} {ev:22} {detail}")

    # ---------------- VERDICT ----------------
    print("\n" + "=" * 78)
    print("VERDICT — does UserPromptSubmit cleanly bracket the summarizer?")
    print("=" * 78)
    # Walk the timeline applying the proposed state machine; flag any case where
    # a NON-summarizer agent POST is seen while ARMED (would be a false capture).
    armed = False
    false_captures = []
    summarizer_captured = 0
    ups_between = None
    last_precompact_t = None
    for t, src, ev, detail in tl:
        if src == "HOOK" and ev == "PreCompact":
            armed = True
            last_precompact_t = t
            ups_between = False
        elif src == "HOOK" and ev == "UserPromptSubmit":
            if armed:
                armed = False  # disarm: a real turn is coming
        elif src == "WIRE" and ev == "POST:SUMMARIZER":
            if armed:
                summarizer_captured += 1
                armed = False
            else:
                print(f"  (!) summarizer POST at t={t} but NOT armed "
                      f"(missed) — last PreCompact t={last_precompact_t}")
        elif src == "WIRE" and ev == "POST:agent":
            if armed:
                false_captures.append(t)
                armed = False
    print(f"  summarizer POSTs correctly captured while armed: {summarizer_captured}")
    print(f"  FALSE captures (real agent POST caught while armed): {len(false_captures)} "
          f"{false_captures}")
    if not false_captures and summarizer_captured >= 1:
        print("  >>> SOUND: UserPromptSubmit disarm prevents catching real turns, "
              "and the summarizer is caught while armed.")
    else:
        print("  >>> NEEDS REVIEW: see flags above.")
    print(f"\n  cap={cap}")


if __name__ == "__main__":
    main()
