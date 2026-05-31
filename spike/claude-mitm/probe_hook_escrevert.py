#!/usr/bin/env python3
"""Probe A: esc-revert observability characterization.

User hypothesis: hitting Esc during the THINK-ONLY phase (before the agent's
first text/tool output) reverts the turn purely in-memory on the Claude Code
side, leaving NO signal AO can capture except TUI scraping. This probe does not
assume that — it CHARACTERIZES which of the four observability channels show
anything, so AO knows whether it reconciles a reverted turn from a positive
signal or must infer it from absence + TUI.

  channel    what we look for
  -------    ----------------
  hook       UserPromptSubmit (fires at submit? before Esc?) and Stop (none on revert?)
  wire       A's request on the proxy, and whether its SSE streamed thinking only
             (=> we tested revert) vs text/tool output (=> we tested interrupt)
  transcript a `user` row for the marker + any chained `assistant` row; snapshotted
             DURING thinking and AGAIN after Esc to tell apart:
                during/after = absent/absent  -> row written only at commit; clean revert
                             = present/absent  -> CC rewrites the JSONL to drop it on revert
                             = present/present -> orphaned row AO must reconcile vs the TUI
  PTY        does the marker text return to the composer / stay in scrollback?

Contrast reference (already observed in prior transcripts): a MID-OUTPUT interrupt
writes a synthetic user row [{type:text,text:"[Request interrupted by user]"}]
plus an `interruptedMessageId` field. The decisive question for revert is whether
think-only Esc writes that too, or nothing.

Determinism: launch with a fresh --session-id so the transcript path is fixed
(no newest-by-mtime race) and --effort high for a long think window. We gate Esc
on "A's request is on the wire" (thinking has started), then dwell ~3s, then Esc.
A post-hoc SSE regime check catches the rare case where output began before Esc.
Driving is one Esc byte (revert) + the harness trust \r + teardown esc.
"""
import os
import time
import json
import uuid

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
CAP = os.environ.get("AO_CAP", "/tmp/ao-cap-danger.jsonl")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-escrevert.log"

SESSION_ID = str(uuid.uuid4())
PROJ = aoprobe.CWD.replace("/", "-")          # /tmp/aocwd -> -tmp-aocwd
TRANSCRIPT = f"/tmp/aoclaude/projects/{PROJ}/{SESSION_ID}.jsonl"
# Unique per run: the shared proxy cap file accumulates across probes, so a
# reused marker would match a PRIOR run's request/SSE and contaminate both the
# Esc timing and the regime check. Derive from the fresh session id.
MARKER = "REVERTMARK" + SESSION_ID.split("-")[0].upper()

# A genuinely-computational question + "think before answering, no tools" keeps
# the model in extended thinking with no text/tool output for several seconds at
# --effort high, which is the window we need to Esc inside of.
PROMPT = (
    f"{MARKER}. Think silently and very thoroughly in your reasoning before "
    "writing anything, and do not use any tools. Enumerate, one at a time in "
    "your reasoning, every ordered triple (a, b, c) of positive integers with "
    "a < b < c and a + b + c = 20, verifying each candidate before moving on, "
    "then count them and state the total."
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


def _content_blob(row):
    c = (row.get("message") or {}).get("content")
    return c if isinstance(c, str) else json.dumps(c)


def user_rows_with(rows, marker):
    return [r for r in rows
            if r.get("type") == "user" and marker in _content_blob(r)]


def assistant_rows(rows):
    return [r for r in rows if r.get("type") == "assistant"]


def interrupted_rows(rows):
    return [r for r in rows
            if "interruptedMessageId" in r or "Request interrupted" in json.dumps(r)]


def last_prompt_row(rows):
    lp = [r for r in rows if r.get("type") == "last-prompt"]
    return lp[-1] if lp else None


def req_ids_with_marker(cap, marker):
    """req_ids of captured REQUESTS whose (raw JSON) body contains the marker."""
    return [r.get("req_id") for r in aoprobe.wire_records(cap)
            if r.get("kind") == "request" and marker in (r.get("body") or "")]


def main():
    aoprobe.seed_config(
        events=["UserPromptSubmit", "Stop", "PreToolUse", "PostToolUse"],
        decision="allow")
    try:
        os.remove(TRANSCRIPT)
    except OSError:
        pass

    sess = aoprobe.ClaudeSession(
        PROMPT, BASE_URL, PTY_LOG,
        extra_args=["--session-id", SESSION_ID, "--effort", "max"])
    sess.start()

    # 1) Esc at the EARLIEST think-only moment: the first thinking chunk on the
    #    wire. Thinking always precedes text/tool output in the SSE stream, so
    #    escaping the instant thinking appears is reliably pre-output. (A fixed
    #    dwell contaminated the first attempt into the interrupt regime — the
    #    model finished thinking inside 3s and began output.)
    req_seen = False
    esc_done = False
    during = []
    esc_at = -1.0
    while sess.elapsed() < 70 and not esc_done:
        sess._pump_once(no_hook_yet=(not aoprobe.payloads()))
        rids = req_ids_with_marker(CAP, MARKER)
        req_seen = req_seen or bool(rids)
        txt = aoprobe.wire_sse_by_req(CAP).get(rids[-1], {}).get("text", "") if rids else ""
        if "thinking" in txt.lower():
            during = read_rows(TRANSCRIPT)      # snapshot the instant before Esc
            esc_at = sess.elapsed()
            sess.send("\x1b")                   # one Esc -> revert (think-only)
            esc_done = True
    if not esc_done:                            # never observed thinking — Esc anyway, note it
        during = read_rows(TRANSCRIPT)
        esc_at = sess.elapsed()
        sess.send("\x1b")

    # 3) let state settle; keep pumping so any post-Esc TUI/hook/transcript writes land
    settle = time.time()
    while time.time() - settle < 6.0:
        sess._pump_once(no_hook_yet=False)

    # 4) snapshots after Esc (before teardown, which sends its own esc)
    after = read_rows(TRANSCRIPT)
    pty_tail = aoprobe._norm(sess._rawtail).decode("ascii", "replace")
    composer_holds_marker = MARKER.lower() in pty_tail

    hook_rows = aoprobe.payloads()
    ups = [e for e in hook_rows if e.get("event") == "UserPromptSubmit"]
    ups_marker = [e for e in ups if MARKER in json.dumps(e.get("payload", {}))]
    stops = [e for e in hook_rows if e.get("event") == "Stop"]

    # wire regime check: did A's response stream text/tool output (=> interrupt)?
    sse = aoprobe.wire_sse_by_req(CAP)
    rids = req_ids_with_marker(CAP, MARKER)
    a_rid = rids[-1] if rids else None
    a_sse = sse.get(a_rid, {}) if a_rid else {}
    sse_text = a_sse.get("text", "")
    streamed_thinking = "thinking" in sse_text.lower()
    streamed_output = any(m in sse_text for m in
                          ('"text_delta"', '"input_json_delta"', '"tool_use"',
                           '"output_text"'))

    keystrokes = sess.keystrokes
    sess.exit()

    during_user = len(user_rows_with(during, MARKER))
    during_asst = len(assistant_rows(during))
    after_user = len(user_rows_with(after, MARKER))
    after_asst = len(assistant_rows(after))
    after_intr = interrupted_rows(after)
    lp = last_prompt_row(after)
    lp_marker = bool(lp and MARKER in json.dumps(lp))

    if during_user and not after_user:
        tcase = "present->absent (CC REWRITES the jsonl to drop the reverted row)"
    elif during_user and after_user:
        tcase = "present->present (ORPHANED user row; AO must reconcile vs TUI)"
    elif not during_user and not after_user:
        tcase = "absent->absent (row written only at commit; reverted turn never persists)"
    else:
        tcase = "absent->present (row appeared after Esc; unexpected — inspect)"

    if streamed_output:
        regime = "INTERRUPT (output streamed before Esc) — rerun with higher --effort"
    elif streamed_thinking:
        regime = "THINK-ONLY (only thinking streamed before Esc) — revert regime confirmed"
    else:
        regime = "UNKNOWN (no stream captured for A — inspect req_seen / cap)"

    print("==== ESC-REVERT OBSERVABILITY (think-only) ====")
    print(f"session-id: {SESSION_ID}")
    print(f"target: 2.1.158 binary  |  effort=max  |  req seen on wire: {req_seen}")
    print(f"esc sent at t={esc_at:.1f}s   keystrokes (trust \\r + this esc + teardown): {keystrokes}")
    print(f"\nREGIME (which behavior we actually exercised): {regime}")
    print(f"   A req_id={a_rid}  sse_len={len(sse_text)}  thinking={streamed_thinking}  output={streamed_output}")

    print("\n-- channel: HOOK --")
    print(f"   UserPromptSubmit total={len(ups)}  with marker={len(ups_marker)}  (when: at submit, before Esc?)")
    print(f"   Stop fired={len(stops)}  (expect 0 on a revert — no normal turn end)")

    print("\n-- channel: TRANSCRIPT --")
    print(f"   marker user-row  during={during_user}  after={after_user}")
    print(f"   assistant rows   during={during_asst}  after={after_asst}")
    print(f"   case: {tcase}")
    print(f"   synthetic interrupt row after Esc: {len(after_intr)}  "
          f"(contrast: mid-output interrupt writes '[Request interrupted by user]')")
    if after_intr:
        print(f"      -> {[_content_blob(r)[:60] for r in after_intr]}")
    print(f"   last-prompt points at marker: {lp_marker}")

    print("\n-- channel: PTY (TUI scrape) --")
    print(f"   marker still visible in composer/scrollback after Esc: {composer_holds_marker}")

    print("\nVERDICT:")
    if streamed_output:
        print("  Tested the INTERRUPT path, not revert. Bump --effort and rerun.")
    elif (not after_user and not after_asst and not after_intr
          and len(stops) == 0):
        sig = "an orphaned UserPromptSubmit" if ups_marker else "NO capturable signal"
        print(f"  Think-only Esc leaves {sig} on hook/wire/transcript.")
        print(f"  Transcript: {tcase.split('(')[0].strip()}.")
        if ups_marker:
            print("  => AO can INFER revert from a SIGNAL (orphaned UPS + no Stop + no commit),")
            print("     not TUI-only. The submit-time UPS is the capture point.")
        else:
            print("  => Matches the in-mem hypothesis: AO must infer revert from ABSENCE")
            print("     (no Stop, no transcript commit) + TUI scrape. No positive event.")
    else:
        print("  Mixed signals — see channels above; transcript/hook left a trace.")
    print("transcript:", TRANSCRIPT)
    print("pty log:", PTY_LOG)
    print("===============================================")


if __name__ == "__main__":
    main()
