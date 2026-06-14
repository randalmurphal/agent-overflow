#!/usr/bin/env python3
"""Resolve compaction DETECTION + REPRESENTATION questions for the claudetui
provider (decision: hook-based structural detection + group the summary under
the compaction divider).

Two questions, one live session on 2.1.170:

  PART A — SAFETY (false-positive risk of a PreCompact latch):
    Does PreCompact fire when /compact is REJECTED for "not enough messages"?
    - 2.1.170 source (compact.ts): the messages.length===0 throw is BEFORE
      executePreCompactHooks, and autoCompact's threshold gates BEFORE
      compactConversation -> PreCompact should fire ONLY when compaction is
      committed (safe for a one-shot latch).
    - HOOKS_COVERAGE_MAP.md claims PreCompact "fires before the enough-messages
      check (so it is not proof compaction happened)" -> would make a latch
      misclassify the user's NEXT real turn (data loss).
    These contradict. Resolve empirically: drive /compact with too little
    context and watch whether PreCompact fires WITHOUT a summarizer POST.

  PART B — SHAPE (needed to capture + group the summary):
    On a real compaction, characterize the summarizer /v1/messages call:
      - request HEADERS: does it carry x-claude-code-agent-id / -parent-agent-id?
        (If it does, AO's gateway would route it as a SUBAGENT, not a main turn;
        if it does NOT, "next non-subagent agent call" cleanly identifies it.)
      - SSE block structure: thinking block? text block? stop_reason? — exactly
        what AO would reconstruct and attach under the "Compacted" divider.
      - ORDERING: PreCompact (hook) -> summarizer (wire) -> PostCompact (hook),
        by timestamp, to confirm the happens-before a latch relies on.

The summary-instruction marker is used HERE ONLY to LOCATE the summarizer
request inside the capture for analysis. Production detection is the hook
latch (structural); the marker stays debug-log-only, mirroring
subagentSystemMarker.

Run:
  PT=$(mktemp -d)
  go build -C proxy -o "$PT/ao-proxy" .
  "$PT/ao-proxy" --listen 127.0.0.1:8097 --log "$PT/cap.jsonl" &
  AO_BASE_URL=http://127.0.0.1:8097 AO_CAP_LOG="$PT/cap.jsonl" python3 probe_compact_detect.py
"""
import json
import os
import time
from collections import Counter
from datetime import datetime

import aoprobe
from probe_compact import req_view, is_agent, agent_end_turns, n_posts, \
    send_line, wait_for_proxy

# Shared prefix of BOTH compaction prompt variants (standard + up_to), verified
# in the 2.1.170 binary string table. ONLY used to locate the summarizer in the
# capture for this analysis — NOT the production detector.
SUMMARY_MARKER = "Your task is to create a detailed summary of"

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
PTY_LOG = "/tmp/aocompact/compact-detect-pty.log"


def precompact_count():
    return sum(1 for p in aoprobe.payloads() if p.get("event") == "PreCompact")


def postcompact_count():
    return sum(1 for p in aoprobe.payloads() if p.get("event") == "PostCompact")


def pty_norm():
    try:
        return aoprobe._norm(open(PTY_LOG, "rb").read()).decode("ascii", "replace")
    except OSError:
        return ""


def parse_proxy_ts(s):
    """RFC3339Nano -> epoch float. Trim to microseconds (Python's %f cap)."""
    if not s:
        return None
    s = s.replace("Z", "+00:00")
    # collapse nanoseconds to microseconds
    if "." in s:
        head, frac = s.split(".", 1)
        tzsign = ""
        for sign in ("+", "-"):
            if sign in frac:
                frac, tz = frac.split(sign, 1)
                tzsign = sign + tz
                break
        frac = frac[:6]
        s = f"{head}.{frac}{tzsign}"
    try:
        return datetime.fromisoformat(s).timestamp()
    except ValueError:
        return None


def find_summarizer_request(cap):
    """Locate the summarizer request record in the raw capture by the marker in
    its message bodies. Returns the full request record (with headers) + req_id,
    or None."""
    for r in aoprobe.wire_records(cap):
        if r.get("kind") != "request" or r.get("path") != "/v1/messages":
            continue
        body = r.get("body") or ""
        if SUMMARY_MARKER in body:
            return r
    return None


def sse_structure(sse_text):
    """Summarize the SSE stream: ordered content blocks + delta-type counts +
    stop_reason. This is the shape AO reconstructs."""
    blocks = []
    deltas = Counter()
    stop_reason = None
    for line in sse_text.splitlines():
        line = line.strip()
        if not line.startswith("data:"):
            continue
        try:
            d = json.loads(line[5:].strip())
        except (json.JSONDecodeError, ValueError):
            continue
        t = d.get("type")
        if t == "content_block_start":
            blocks.append(d.get("content_block", {}).get("type"))
        elif t == "content_block_delta":
            deltas[d.get("delta", {}).get("type")] += 1
        elif t == "message_delta":
            sr = d.get("delta", {}).get("stop_reason")
            if sr:
                stop_reason = sr
    return blocks, dict(deltas), stop_reason


def interesting_headers(headers):
    """Non-credential headers relevant to routing identity."""
    out = {}
    for k, v in (headers or {}).items():
        lk = k.lower()
        if ("claude-code" in lk or "agent" in lk or lk == "anthropic-beta"
                or lk == "user-agent" or "session" in lk):
            out[k] = v
    return out


def main():
    cap = os.environ["AO_CAP_LOG"]
    base = os.environ["AO_BASE_URL"]
    wait_for_proxy(base)
    os.makedirs("/tmp/aocompact", exist_ok=True)

    aoprobe.seed_config(["PreCompact", "PostCompact", "SessionStart", "Stop"])

    sess = aoprobe.ClaudeSession(
        PROMPTS[0], base, PTY_LOG, extra_args=["--model", "haiku"])
    sess.start()

    # One real turn so the session is warm (turn 1 is the positional auto-submit).
    print("[A] warming session with 1 turn ...", flush=True)
    sess.run(until=lambda: len(agent_end_turns(req_view(cap))) >= 1, max_s=180)
    print(f"    agent_turns={len(agent_end_turns(req_view(cap)))}", flush=True)

    # ---------------- PART A: rejected /compact ----------------
    posts_before_A = n_posts(cap)
    pre_before_A = precompact_count()
    print(f"[A] /compact with minimal context "
          f"(posts={posts_before_A} pre={pre_before_A}) ...", flush=True)
    send_line(sess, "/compact")
    # A real compaction adds a summarizer POST; "not enough messages" adds none.
    got_post_A = sess.run(
        until=lambda: n_posts(cap) > posts_before_A
        and (time.time() - sess._last_out) > 2.5, max_s=25)
    time.sleep(1.0)
    pre_after_A = precompact_count()
    pty_A = pty_norm()
    not_enough = "notenoughmessages" in pty_A or "messagestocompact" in pty_A
    print(f"[A] RESULT: summarizer_post={got_post_A} "
          f"posts={posts_before_A}->{n_posts(cap)} "
          f"PreCompact {pre_before_A}->{pre_after_A} "
          f"'not enough' on screen={not_enough}", flush=True)
    phaseA = {
        "summarizer_post": got_post_A,
        "precompact_fired": pre_after_A > pre_before_A,
        "not_enough_on_screen": not_enough,
    }

    # ---------------- PART B: real compaction ----------------
    print(f"[B] building context: {len(PROMPTS)} turns ...", flush=True)
    for k, p in enumerate(PROMPTS, start=1):
        if k > 1:
            send_line(sess, p)
            sess.run(until=lambda k=k: len(agent_end_turns(req_view(cap))) >= k,
                     max_s=200)
    print(f"    agent_turns={len(agent_end_turns(req_view(cap)))}", flush=True)

    posts_before_B = n_posts(cap)
    pre_before_B = precompact_count()
    post_before_B = postcompact_count()
    print(f"[B] /compact (posts={posts_before_B}) ...", flush=True)
    send_line(sess, "/compact")
    got_post_B = sess.run(
        until=lambda: n_posts(cap) > posts_before_B
        and (time.time() - sess._last_out) > 2.5, max_s=360)
    time.sleep(1.5)
    print(f"[B] compaction: summarizer_post={got_post_B} "
          f"PreCompact {pre_before_B}->{precompact_count()} "
          f"PostCompact {post_before_B}->{postcompact_count()}", flush=True)

    time.sleep(1.0)
    sess.exit()

    # ---------------- ANALYSIS ----------------
    print("\n" + "=" * 72)
    print("PART A — does PreCompact fire on a REJECTED /compact?")
    print("=" * 72)
    print(json.dumps(phaseA, indent=2))
    if phaseA["precompact_fired"] and not phaseA["summarizer_post"]:
        print(">>> UNSAFE signal: PreCompact fired with NO summarizer POST.")
        print(">>> A one-shot latch would catch the next REAL turn. Needs a guard.")
    elif not phaseA["summarizer_post"] and not phaseA["precompact_fired"]:
        print(">>> SAFE signal: rejected /compact fired NO PreCompact "
              "(matches 2.1.170 source; AO's stale comment is wrong).")
    else:
        print(">>> Minimal context still COMPACTED (floor <= 1 turn); empty-case "
              "is only literal 0 messages -> source throws before PreCompact.")

    print("\n" + "=" * 72)
    print("PART B — summarizer request shape")
    print("=" * 72)
    sumreq = find_summarizer_request(cap)
    if not sumreq:
        print("  (!) summarizer request NOT found by marker — inspect capture:",
              cap)
    else:
        rid = sumreq.get("req_id")
        hdrs = interesting_headers(sumreq.get("headers"))
        has_agent = any("agent-id" in k.lower() for k in (sumreq.get("headers") or {}))
        try:
            body = json.loads(sumreq.get("body") or "{}")
        except (json.JSONDecodeError, ValueError):
            body = {}
        print(f"  req_id={rid}")
        print(f"  max_tokens={body.get('max_tokens')}  n_tools={len(body.get('tools', []))}")
        print(f"  carries x-claude-code-agent-id? {has_agent}")
        print(f"  identity headers: {json.dumps(hdrs, indent=2)}")
        sse = aoprobe.wire_sse_by_req(cap).get(rid, {})
        blocks, deltas, stop_reason = sse_structure(sse.get("text", ""))
        print(f"  SSE blocks (in order): {blocks}")
        print(f"  SSE delta counts: {deltas}")
        print(f"  stop_reason: {stop_reason}  ended={sse.get('ended')} "
              f"status={sse.get('status')}")
        # where is the marker — last user message?
        msgs = body.get("messages", [])
        roles = [m.get("role") for m in msgs]
        last_user_has = False
        for m in reversed(msgs):
            if m.get("role") == "user":
                c = m.get("content")
                txt = c if isinstance(c, str) else json.dumps(c)
                last_user_has = SUMMARY_MARKER in txt
                break
        print(f"  n_msgs={len(msgs)} roles_tail={roles[-4:]} "
              f"marker_in_LAST_user_msg={last_user_has}")

    print("\n" + "=" * 72)
    print("PART B — ordering: PreCompact -> summarizer -> PostCompact")
    print("=" * 72)
    pre_ts = [p["ts"] for p in aoprobe.payloads() if p.get("event") == "PreCompact"]
    post_ts = [p["ts"] for p in aoprobe.payloads() if p.get("event") == "PostCompact"]
    sum_ts = None
    if sumreq:
        sum_ts = parse_proxy_ts(sumreq.get("ts"))
    print(f"  PreCompact  ts: {pre_ts}")
    print(f"  summarizer  ts: {sum_ts}")
    print(f"  PostCompact ts: {post_ts}")
    if pre_ts and sum_ts and post_ts:
        last_pre = max(t for t in pre_ts if t <= sum_ts) if any(t <= sum_ts for t in pre_ts) else pre_ts[-1]
        post_after = [t for t in post_ts if t >= sum_ts]
        print(f"  PreCompact before summarizer? {last_pre <= sum_ts} "
              f"(delta={sum_ts - last_pre:.3f}s)")
        if post_after:
            print(f"  PostCompact after summarizer? True "
                  f"(delta={post_after[0] - sum_ts:.3f}s)")

    print("\n" + "=" * 72)
    print("HOOK PAYLOAD DETAIL (PreCompact / PostCompact)")
    print("=" * 72)
    for p in aoprobe.payloads():
        if p.get("event") in ("PreCompact", "PostCompact"):
            pl = p.get("payload", {})
            print(f"  {p['event']}: keys={list(pl.keys())} "
                  f"trigger={pl.get('trigger')}")
            print(f"     {json.dumps(pl)[:400]}")
    print(f"\n[keystrokes={sess.keystrokes}] cap={cap}")


if __name__ == "__main__":
    main()
