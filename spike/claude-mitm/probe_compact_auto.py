#!/usr/bin/env python3
"""Validate the signal-based compaction detector for AUTO-compaction (the common
real-world trigger), not just manual /compact.

Auto-compaction fires at the context boundary: the user submits a prompt, Claude
Code notices it's over threshold, compacts FIRST, then runs the real turn. The
expected ordering is:

    UserPromptSubmit(real)  ->  PreCompact(trigger=auto)  ->  POST(summarizer)
        ->  PostCompact  ->  POST(real turn)

The risk vs. the manual case: the disarming UserPromptSubmit happens BEFORE
PreCompact arms. So the safety of "armed POST with no intervening
UserPromptSubmit = summarizer" depends on the summarizer being the FIRST agent
POST after PreCompact (so it consumes the latch before the real turn's POST).
This probe forces an early auto-compaction and checks exactly that.

Lever: CLAUDE_CODE_AUTO_COMPACT_WINDOW shrinks the context window so the
threshold (window - 20000 - 13000) is crossed after a few context-growing turns.

Run:
  PT=$(mktemp -d)
  go build -C proxy -o "$PT/ao-proxy" .
  "$PT/ao-proxy" --listen 127.0.0.1:8099 --log "$PT/cap.jsonl" &
  AO_BASE_URL=http://127.0.0.1:8099 AO_CAP_LOG="$PT/cap.jsonl" python3 probe_compact_auto.py
"""
import json
import os
import time

import aoprobe
from probe_compact import req_view, agent_end_turns, n_posts, send_line, \
    wait_for_proxy

MARKER = "Your task is to create a detailed summary of"
# Auto-compact compares tokenCountWithEstimation(MESSAGES) (NOT system+tools) to
# the threshold. So shrink the window AND force a low % so the message-token
# threshold is small, then grow messages with long outputs:
#   effectiveWindow = 39000 - 20000(summary) = 19000
#   threshold = 19000 - 13000(buffer) = 6000 msg tokens
# Haiku writes terse prose, so "essay" prompts accumulated too slowly. These
# prompts force DETERMINISTIC long output (~1200-1800 tokens each), so a handful
# of turns cross 6000. Post-compaction floor (summary ~2500-4000 + reminders)
# stays under 6000, and the tiny "READY" follow-up keeps it there — no
# re-cascade. 12 prompts so it still fires even if haiku truncates the lists.
AUTO_COMPACT_WINDOW = "39000"
PTY_LOG = "/tmp/aocompact/compact-auto-pty.log"

# Forced large deterministic outputs (haiku can't shorten these the way it
# trimmed the essays).
GROW = [
    "Output the integers 1 through 200, one per line, each formatted exactly as 'N => <english spelling of N>'.",
    "List 150 distinct common English nouns, numbered 1-150, one per line, each followed by a short 6-word example sentence.",
    "Output the integers 201 through 400, one per line, each formatted exactly as 'N => <english spelling of N>'.",
    "List 150 distinct common English verbs, numbered 1-150, one per line, each followed by a short 6-word example sentence.",
    "Output the integers 401 through 600, one per line, each formatted exactly as 'N => <english spelling of N>'.",
    "List 150 distinct English adjectives, numbered 1-150, one per line, each followed by a short 6-word example sentence.",
    "Output the integers 601 through 800, one per line, each formatted exactly as 'N => <english spelling of N>'.",
    "List 150 distinct English adverbs, numbered 1-150, one per line, each followed by a short 6-word example sentence.",
    "Output the integers 801 through 1000, one per line, each formatted exactly as 'N => <english spelling of N>'.",
    "List 150 distinct country or city names, numbered 1-150, one per line, each with a 6-word fact.",
    "Output the integers 1001 through 1200, one per line, each formatted exactly as 'N => <english spelling of N>'.",
    "List 150 distinct animal names, numbered 1-150, one per line, each with a 6-word fact.",
]


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


SUGGESTION_MARKER = "[SUGGESTION MODE:"


def last_user_text(b):
    for m in reversed(b.get("messages", [])):
        if m.get("role") != "user":
            continue
        c = m.get("content")
        if isinstance(c, str):
            return c
        if isinstance(c, list):
            return " ".join(blk.get("text", "") for blk in c
                            if isinstance(blk, dict) and blk.get("type") == "text")
    return ""


def classify_post(body_raw):
    """Mirror classify.go's partitioning so the state-machine verdict reflects
    what PRODUCTION would actually feed the latch (only classAgent counts)."""
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
    if last_user_text(b).lstrip().startswith(SUGGESTION_MARKER):
        return "suggestion", b
    return "agent", b


# Session start (epoch seconds, time.time()). The proxy appends to cap.jsonl, so
# a stale capture from a prior run can contaminate it. The proxy's RFC3339-UTC
# timestamps and Python's time.time() are both UTC-epoch and directly
# comparable (verified: this run's wire POSTs align with this run's hooks). So
# we discard any wire record older than the session start — detection then sees
# ONLY this run's traffic regardless of capture-file reuse.
SESSION_T0 = None
WIRE_EPSILON = 2.0  # grace for records that land microseconds before t0


def _wire_after_start(r):
    if SESSION_T0 is None:
        return True
    ts = parse_proxy_ts(r.get("ts"))
    return ts is None or ts >= SESSION_T0 - WIRE_EPSILON


def auto_precompact_fired():
    return any(p.get("event") == "PreCompact"
               and (p.get("payload", {}) or {}).get("trigger") == "auto"
               for p in aoprobe.payloads())


def postcompact_count():
    return sum(1 for p in aoprobe.payloads() if p.get("event") == "PostCompact")


def summarizer_seen(cap):
    return any(r.get("kind") == "request" and MARKER in (r.get("body") or "")
               and _wire_after_start(r)
               for r in aoprobe.wire_records(cap))


def unified_timeline(cap, t0):
    events = []
    for p in aoprobe.payloads():
        ev = p.get("event")
        if ev in ("UserPromptSubmit", "PreCompact", "PostCompact", "Notification",
                  "SessionStart", "Stop"):
            pl = p.get("payload", {}) or {}
            detail = ""
            if ev == "UserPromptSubmit":
                detail = (pl.get("prompt") or "")[:34].replace("\n", " ")
            elif ev == "PostCompact":
                detail = f"summary_len={len(pl.get('compact_summary') or '')}"
            elif ev == "PreCompact":
                detail = f"trigger={pl.get('trigger')}"
            events.append((p.get("ts", 0), "HOOK", ev, detail))
    for r in aoprobe.wire_records(cap):
        if (r.get("kind") == "request" and r.get("path") == "/v1/messages"
                and _wire_after_start(r)):
            ts = parse_proxy_ts(r.get("ts"))
            kind, b = classify_post(r.get("body"))
            events.append((ts or 0, "WIRE", f"POST:{kind}",
                           f"max_tok={b.get('max_tokens')} n_tools={len(b.get('tools', []))}"))
    events.sort(key=lambda e: e[0])
    return [(round(ts - t0, 3), src, ev, d) for ts, src, ev, d in events]


def main():
    cap = os.environ["AO_CAP_LOG"]
    base = os.environ["AO_BASE_URL"]
    wait_for_proxy(base)
    os.makedirs("/tmp/aocompact", exist_ok=True)

    aoprobe.seed_config(["PreCompact", "PostCompact", "UserPromptSubmit",
                         "Notification", "SessionStart", "Stop"])
    # Ensure auto-compact is enabled in the seeded global config.
    cfg_path = f"{aoprobe.CONFIG_DIR}/.claude.json"
    try:
        gc = json.load(open(cfg_path))
    except (OSError, json.JSONDecodeError):
        gc = {}
    gc["autoCompactEnabled"] = True
    json.dump(gc, open(cfg_path, "w"))

    sess = aoprobe.ClaudeSession(
        GROW[0], base, PTY_LOG, extra_args=["--model", "haiku"],
        extra_env={"CLAUDE_CODE_AUTO_COMPACT_WINDOW": AUTO_COMPACT_WINDOW})
    sess.start()
    t0 = time.time()
    global SESSION_T0
    SESSION_T0 = t0  # discard any pre-existing (stale) wire records in cap.jsonl

    print(f"[auto] window={AUTO_COMPACT_WINDOW} -> threshold ~6000 MESSAGE tokens; "
          f"growing context until auto-compact ...", flush=True)
    fired_at = None
    for k in range(1, len(GROW) + 1):
        if k > 1:
            send_line(sess, GROW[k - 1])
        # run until this turn settles (idle) — auto-compact, if any, happens inside
        sess.run(until=lambda k=k: len(agent_end_turns(req_view(cap))) >= k
                 and (time.time() - sess._last_out) > 2.0, max_s=240)
        fired = auto_precompact_fired() or summarizer_seen(cap)
        print(f"   turn {k}: posts={n_posts(cap)} "
              f"auto_precompact={auto_precompact_fired()} "
              f"summarizer_seen={summarizer_seen(cap)}", flush=True)
        if fired:
            fired_at = k
            break

    if fired_at is None:
        print("[auto] !! auto-compaction did NOT fire within the turn budget — "
              "lower CLAUDE_CODE_AUTO_COMPACT_WINDOW and retry.", flush=True)
    else:
        # Let the compaction SETTLE (wait for PostCompact) before the follow-up,
        # so the follow-up turn is cleanly AFTER the whole compaction sequence.
        print(f"[auto] detected at turn {fired_at}; waiting for PostCompact ...",
              flush=True)
        sess.run(until=lambda: postcompact_count() > 0
                 and (time.time() - sess._last_out) > 2.0, max_s=90)
        print(f"[auto] PostCompact count={postcompact_count()}; "
              f"one clean follow-up turn ...", flush=True)
        tb = len(agent_end_turns(req_view(cap)))
        send_line(sess, "Reply with only the single word READY.")
        sess.run(until=lambda: len(agent_end_turns(req_view(cap))) > tb
                 and (time.time() - sess._last_out) > 2.0, max_s=200)

    time.sleep(1.0)
    sess.exit()

    # ---------------- TIMELINE around the compaction ----------------
    tl = unified_timeline(cap, t0)
    print("\n" + "=" * 78)
    print("UNIFIED TIMELINE (auto-compaction run)")
    print("=" * 78)
    # Print a window around the first PreCompact for readability.
    pre_idx = next((i for i, e in enumerate(tl)
                    if e[1] == "HOOK" and e[2] == "PreCompact"), None)
    lo = max(0, (pre_idx or 0) - 4)
    hi = min(len(tl), (pre_idx or 0) + 12) if pre_idx is not None else len(tl)
    if pre_idx is None:
        print("  (no PreCompact in timeline)")
    for t, src, ev, d in tl[lo:hi]:
        tag = "  HOOK" if src == "HOOK" else "WIRE  "
        print(f"  [{t:8.3f}] {tag} {ev:20} {d}")

    # ---------------- VERDICT ----------------
    print("\n" + "=" * 78)
    print("VERDICT — state machine on the AUTO-compaction timeline")
    print("=" * 78)
    armed = False
    false_caps, summ_caps = [], 0
    pre_trigger = None
    for t, src, ev, d in tl:
        if src == "HOOK" and ev == "PreCompact":
            armed = True
            pre_trigger = d
        elif src == "HOOK" and ev == "UserPromptSubmit":
            armed = False  # a real user turn is next
        elif src == "HOOK" and ev == "PostCompact":
            armed = False  # compaction fully settled
        elif src == "WIRE" and ev == "POST:SUMMARIZER":
            if armed:
                summ_caps += 1
                armed = False
            else:
                print(f"  (!) summarizer POST at t={t} while NOT armed (missed)")
        elif src == "WIRE" and ev == "POST:agent":
            if armed:
                false_caps.append(t)
                armed = False
        # POST:suggestion / POST:quota / POST:aux are filtered by classify.go —
        # ignored here (they never reach the latch in production).
    print(f"  PreCompact trigger seen: {pre_trigger}")
    print(f"  summarizer captured while armed: {summ_caps}")
    print(f"  FALSE captures (real agent POST caught while armed): {len(false_caps)} {false_caps}")
    if summ_caps >= 1 and not false_caps:
        print("  >>> SOUND for auto-compaction: summarizer caught, no real turn mis-captured.")
    else:
        print("  >>> NEEDS REVIEW.")
    print(f"\n  cap={cap}")


if __name__ == "__main__":
    main()
