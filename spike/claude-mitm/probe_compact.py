#!/usr/bin/env python3
"""Characterize CONTEXT COMPACTION in interactive TUI mode (the spike's one
'cleanest genuine omission — never probed' item, HOOKS_COVERAGE_MAP.md §open).

Question AO needs answered: when a long thread compacts, what is the signature
on the three taps AO actually has (gateway wire / transcript / hooks)? The
headless provider gets a clean typed `system/compact_boundary` envelope, but
that is Claude Code's own stream-json synthesis and is NOT on the raw wire.

Method (cheap + deterministic — no waiting for a big session):
  1. one short haiku turn to put SOMETHING in the context,
  2. drive `/compact` in the live TUI to force a summarization,
  3. one short follow-up turn to FLUSH the post-compaction context onto the wire
     (so we can see whether history was replaced by a summary),
  4. report the wire requests, the transcript rows, and whether the documented
     PreCompact/PostCompact hooks fired (the 'hook #2' question).

Run via run_compact.sh (starts the proxy, sets AO_BASE_URL / AO_CAP_LOG).
"""
import json
import os
import time

import aoprobe

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
FOLLOWUP = "Reply with only the single word READY and nothing else."
PTY_LOG = "/tmp/aocompact/compact-pty.log"


def send_line(sess, text):
    """Type a line, let the composer render, then submit (paste/enter coalescing
    drops a \\r sent in the same instant as the text — see steering probe)."""
    sess.send(text)
    time.sleep(0.5)
    sess.send("\r")


# ---- combined per-request wire view (req_id -> body + SSE) ----
def req_view(cap):
    bodies, sse = {}, aoprobe.wire_sse_by_req(cap)
    order = []
    for r in aoprobe.wire_records(cap):
        if r.get("kind") == "request":
            rid = r.get("req_id")
            if rid not in bodies:
                order.append(rid)
            try:
                bodies[rid] = {"path": r.get("path"),
                               "body": json.loads(r["body"]) if r.get("body") else {}}
            except (json.JSONDecodeError, ValueError):
                bodies[rid] = {"path": r.get("path"), "body": {}}
    out = []
    for rid in order:
        b = bodies[rid]["body"]
        s = sse.get(rid, {"text": "", "status": None, "ended": False})
        msgs = b.get("messages", []) if isinstance(b, dict) else []
        tools = b.get("tools", []) if isinstance(b, dict) else []
        stops = []
        for tok in ('"stop_reason":"end_turn"', '"stop_reason":"tool_use"',
                    '"stop_reason":"max_tokens"', '"stop_reason":"stop_sequence"'):
            if tok in s["text"]:
                stops.append(tok.split('"')[3])
        out.append({
            "rid": rid, "path": bodies[rid]["path"],
            "max_tokens": b.get("max_tokens") if isinstance(b, dict) else None,
            "n_tools": len(tools), "n_msgs": len(msgs),
            "roles": [m.get("role") for m in msgs] if msgs else [],
            "status": s["status"], "ended": s["ended"],
            "has_message_stop": "message_stop" in s["text"],
            "stops": stops, "body": b,
        })
    return out


def is_agent(r):
    """Real main-loop turn: populated tools + not the 1-token quota preflight."""
    return r["n_tools"] > 0 and (r["max_tokens"] or 0) > 1


def agent_end_turns(view):
    return [r for r in view if is_agent(r) and "end_turn" in r["stops"] and r["ended"]]


def n_posts(cap):
    """POST /v1/messages count — the summarization call adds one; 'not enough
    messages' adds none, so this is the did-it-actually-compact signal."""
    return sum(1 for r in req_view(cap) if r["path"] == "/v1/messages")


def msg_snip(m, n=140):
    c = m.get("content")
    if isinstance(c, str):
        return c[:n].replace("\n", " ")
    if isinstance(c, list):
        parts = []
        for blk in c:
            if not isinstance(blk, dict):
                continue
            t = blk.get("type")
            if t == "text":
                parts.append("text:" + blk.get("text", "")[:n])
            else:
                parts.append(t or "?")
        return (" | ".join(parts))[:n].replace("\n", " ")
    return str(c)[:n]


def wait_for_proxy(base, timeout=10.0):
    """Block until the loopback gateway accepts a connection (avoids a shell
    `sleep` for startup; the proxy is backgrounded in the same shell call)."""
    import socket
    from urllib.parse import urlparse
    u = urlparse(base)
    host, port = u.hostname, u.port or 80
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection((host, port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.2)
    raise SystemExit(f"proxy at {base} never came up")


def main():
    cap = os.environ["AO_CAP_LOG"]
    base = os.environ["AO_BASE_URL"]
    wait_for_proxy(base)

    # Register the documented compaction hooks so we learn whether they fire on a
    # manual /compact (the 'is PreCompact a clean hook #2' question), plus a couple
    # of lifecycle events for context. decision=allow is irrelevant here (full
    # access via --dangerously-skip-permissions), but harmless.
    aoprobe.seed_config(["PreCompact", "PostCompact", "SessionStart", "Stop"])

    # Default mode (no --dangerously-skip-permissions): compaction is not
    # permission-gated and the pure-text prompts trigger no tools, so default
    # mode needs no approval — and it avoids the one-time bypass-acknowledgment
    # Select screen (whose default row is "No, exit") that a blind submit-nudge
    # would otherwise dismiss into an exit.
    sess = aoprobe.ClaudeSession(
        PROMPTS[0], base, PTY_LOG, extra_args=["--model", "haiku"])
    sess.start()

    # Build enough context to clear the "Not enough messages to compact" floor:
    # several short turns (turn 1 is the positional auto-submit).
    print(f"[drive] building context: {len(PROMPTS)} short turns ...", flush=True)
    for k, p in enumerate(PROMPTS, start=1):
        if k > 1:
            send_line(sess, p)
        ok = sess.run(
            until=lambda k=k: len(agent_end_turns(req_view(cap))) >= k, max_s=300)
        print(f"   turn {k}/{len(PROMPTS)} done={ok} "
              f"agent_turns={len(agent_end_turns(req_view(cap)))}", flush=True)
        if not ok:
            break

    # Force compaction. Real compaction makes its OWN POST /v1/messages (the
    # summarization call); 'not enough messages' makes none — so POST growth is
    # the did-it-actually-compact signal (PreCompact fires either way).
    posts_before = n_posts(cap)
    print(f"[drive] /compact (posts so far={posts_before}) ...", flush=True)
    send_line(sess, "/compact")
    ok2 = sess.run(
        until=lambda: n_posts(cap) > posts_before
        and (time.time() - sess._last_out) > 2.5, max_s=360)
    pre = any(p.get("event") == "PreCompact" for p in aoprobe.payloads())
    print(f"[drive] compact done={ok2} posts_now={n_posts(cap)} "
          f"precompact_fired={pre}", flush=True)

    # Follow-up turn flushes the post-compaction context onto the wire.
    after = len(req_view(cap))
    print("[drive] follow-up turn ...", flush=True)
    send_line(sess, FOLLOWUP)
    ok3 = sess.run(
        until=lambda: len(req_view(cap)) > after and req_view(cap)[-1]["ended"]
        and (time.time() - sess._last_out) > 2.5, max_s=400)
    print(f"[drive] follow-up done={ok3}", flush=True)

    time.sleep(1.0)
    sess.exit()

    # ---------------- ANALYSIS ----------------
    print("\n" + "=" * 72)
    print("WIRE: every captured request, in order")
    print("=" * 72)
    view = req_view(cap)
    for i, r in enumerate(view):
        kind = "agent" if is_agent(r) else (
            "quota" if (r["max_tokens"] or 0) <= 1 else (
                "title/aux(tools=0)" if r["n_tools"] == 0 else "?"))
        print(f"[{i:2}] {kind:18} path={r['path']} status={r['status']} "
              f"max_tok={r['max_tokens']} n_tools={r['n_tools']} "
              f"n_msgs={r['n_msgs']} stops={r['stops']} "
              f"msg_stop={r['has_message_stop']} roles={r['roles']}")

    agents = [r for r in view if is_agent(r)]
    print("\n" + "=" * 72)
    print(f"AGENT requests: {len(agents)}  (compare messages[] first vs last)")
    print("=" * 72)
    for label, r in (("FIRST agent", agents[0] if agents else None),
                     ("LAST agent", agents[-1] if agents else None)):
        if not r:
            continue
        print(f"\n--- {label}: n_msgs={r['n_msgs']} roles={r['roles']}")
        for j, m in enumerate(r["body"].get("messages", [])):
            print(f"    [{j}] {m.get('role'):9} {msg_snip(m)}")
        # system prompt can carry the compaction summary marker too
        sysv = r["body"].get("system")
        if isinstance(sysv, list):
            for blk in sysv:
                txt = blk.get("text", "") if isinstance(blk, dict) else str(blk)
                if any(w in txt.lower() for w in ("summar", "compact", "recap",
                                                  "previous conversation")):
                    print(f"    SYSTEM(summary-ish): {txt[:200]}")

    print("\n" + "=" * 72)
    print("TRANSCRIPT: rows + any compaction markers")
    print("=" * 72)
    import glob
    tx = sorted(glob.glob(f"{aoprobe.CONFIG_DIR}/projects/*/*.jsonl"),
                key=lambda p: os.path.getmtime(p))
    if not tx:
        print("  (no transcript found under", aoprobe.CONFIG_DIR, ")")
    else:
        path = tx[-1]
        print(f"  file: {path}")
        rows = []
        for ln in open(path, errors="replace"):
            try:
                rows.append(json.loads(ln))
            except (json.JSONDecodeError, ValueError):
                continue
        from collections import Counter
        types = Counter((r.get("type"), r.get("subtype")) for r in rows)
        print(f"  rows={len(rows)}  type/subtype counts:")
        for k, n in types.most_common():
            print(f"     {k}: {n}")
        print("  rows mentioning 'compact' / isCompactSummary / summary:")
        for i, r in enumerate(rows):
            blob = json.dumps(r).lower()
            if ("compact" in blob or r.get("isCompactSummary")
                    or '"summary"' in blob or "compactmetadata" in blob):
                keys = list(r.keys())
                print(f"     [{i}] type={r.get('type')} subtype={r.get('subtype')} "
                      f"isCompactSummary={r.get('isCompactSummary')} keys={keys}")
                print(f"         {json.dumps(r)[:300]}")

    print("\n" + "=" * 72)
    print("HOOKS: did PreCompact / PostCompact fire?")
    print("=" * 72)
    for p in aoprobe.payloads():
        ev = p.get("event")
        if ev in ("PreCompact", "PostCompact"):
            pl = p.get("payload", {})
            print(f"  {ev}: keys={list(pl.keys())} trigger={pl.get('trigger')} "
                  f"custom={pl.get('custom_instructions')}")
            print(f"     {json.dumps(pl)[:300]}")
    seen = [e for (e, _t) in aoprobe.events_seen()]
    from collections import Counter
    print("  all hook events seen:", dict(Counter(seen)))
    print(f"\n[keystrokes written to PTY: {sess.keystrokes}]  cap={cap}")


if __name__ == "__main__":
    main()
