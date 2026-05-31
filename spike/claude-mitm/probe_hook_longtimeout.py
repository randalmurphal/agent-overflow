#!/usr/bin/env python3
"""Probe: does a GENEROUS settings `timeout` let a gate hook hold the tool long
enough for a human to approve (the >30s concern), overriding Claude's default
(~60s) hook timeout?

Rule 1 established that a KILLED hook (2s timeout) falls through. The open
question for a human-in-the-loop approval flow is the opposite end: can we set a
timeout big enough that a slow human approval still lands? We set timeout=120s
and have the relay hold ~70s before returning allow.

  survived: relay FINISHED marker with since_entry ~70 + no native prompt + tool
    ran  ->  the 120s timeout was honored; a 70s human approval is supported
    (so >30s is fine — configure the timeout generously).
  clamped:  no FINISHED + native prompt (default mode) + tool not run  ->  the
    hold was cut below 70s; AO cannot wait that long for a human here.

Uses defaultMode=default so that WITHOUT the returned allow the echo would
prompt; the tool running with NO prompt proves the held hook's allow applied.
"""
import os

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
OUT = f"{aoprobe.AOHOOK}/lt_echo.txt"
PTY_LOG = f"{aoprobe.AOHOOK}/pty-longtimeout.log"
HOLD = 70.0
TIMEOUT = 120
PROMPT = ("Use the Bash tool to run exactly this command and nothing else: "
          f"echo LT-OK > {OUT}")


def main():
    aoprobe.seed_config(events=["PreToolUse", "PostToolUse"], decision="allow",
                        sleep_s=HOLD, timeout_s=TIMEOUT, default_mode="default")
    try:
        os.remove(OUT)
    except OSError:
        pass
    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()
    # Disable the submit-nudge (no_hook_probe=lambda: False) so a stray Enter can
    # never accept a native prompt if the hold were clamped.
    sess.run(until=lambda: os.path.exists(OUT) or sess.saw_tui_perm,
             max_s=HOLD + 45, no_hook_probe=lambda: False)
    sess._drain(3.0)
    sess.exit()

    rows = aoprobe.payloads()
    fin = [e for e in rows if e["event"] == "PreToolUse-FINISHED"]
    held = round(fin[0].get("since_entry"), 1) if fin and fin[0].get("since_entry") else None
    post = [e for e in rows if e["event"] == "PostToolUse" and e.get("tool") == "Bash"]
    ran = os.path.exists(OUT)

    print("==== LONG-TIMEOUT (human-approval window) PROBE ====")
    print(f"configured timeout: {TIMEOUT}s   relay hold: {HOLD}s")
    print(f"hook survived (FINISHED marker): {bool(fin)}")
    print(f"held_s (since_entry): {held}")
    print(f"native prompt appeared: {sess.saw_tui_perm}")
    print(f"PostToolUse fired: {bool(post)}")
    print(f"tool ran (echo wrote file): {ran}")
    if fin and held and held >= HOLD - 5 and ran and not sess.saw_tui_perm:
        verdict = (f"SUPPORTED: a {TIMEOUT}s timeout honored a {held}s hold and the held "
                   "allow applied — a human taking >30s to approve works (configure the "
                   "timeout generously; the relay still owns a deadline under it).")
    elif not fin and sess.saw_tui_perm:
        verdict = ("CLAMPED: the hold was killed before completing; the timeout did not "
                   "extend the window — long human approval NOT supported at this value.")
    else:
        verdict = "INCONCLUSIVE — inspect markers above."
    print(f"\nVERDICT: {verdict}")
    print("pty log:", PTY_LOG)
    print("====================================================")


if __name__ == "__main__":
    main()
