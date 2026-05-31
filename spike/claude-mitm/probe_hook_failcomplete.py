#!/usr/bin/env python3
"""Probe: how is a FAILED tool completion (non-zero exit) signalled?

A first run revealed a Bash call that exits non-zero does NOT fire PostToolUse
(an exit-0 control fired it immediately; the exit-3 command did not). The Claude
source (v2.1.88) shows why — there is a SEPARATE hook event, `PostToolUseFailure`
(src/types/hooks.ts), distinct from `PostToolUse` (success). So failed
completions have their OWN hook signal; it is just a different event AO must
register. PostToolUse is a SUCCESS signal, not a FINISHED signal.

This probe registers PreToolUse + PostToolUse + PostToolUseFailure, runs a
foreground Bash that writes a known stderr marker and exits 3, and asserts:
  - PostToolUseFailure FIRES (carrying the failure info), and
  - PostToolUse does NOT fire (the success/failure discriminator), and
  - the model-facing failure (stderr + is_error) is ALSO recoverable from the
    wire tool_result in the following request (belt-and-suspenders).

Distinct from an INTERRUPT (Rule 3: NO Post* hook fires at all) — a non-zero
exit is a real completion, just an unsuccessful one.
"""
import json
import os

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
CAP = os.environ.get("AO_CAP_LOG", "/tmp/ao-cap.jsonl")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-failcomplete.log"
MARK = "OOPS-STDERR-7f3a"
PROMPT = ("Use the Bash tool to run exactly this command and nothing else, in the "
          f"FOREGROUND (do not background it): sh -c 'echo {MARK} >&2; exit 3'")


def main():
    aoprobe.seed_config(events=["PreToolUse", "PostToolUse", "PostToolUseFailure"],
                        decision="allow")
    open(CAP, "w").close()
    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()
    sess.run(until=lambda: any(e["event"] in ("PostToolUse", "PostToolUseFailure")
                               for e in aoprobe.payloads()),
             max_s=70)
    sess._drain(5.0)
    sess.exit()

    rows = aoprobe.payloads()
    fail = [e for e in rows if e["event"] == "PostToolUseFailure"]
    succ = [e for e in rows if e["event"] == "PostToolUse" and e.get("tool") == "Bash"]
    fail_payload = fail[0]["payload"] if fail else {}
    hook_has_marker = MARK in json.dumps(fail_payload)

    # Model-facing failure info also lands on the wire as the tool_result in the
    # following request body.
    bodies = aoprobe.wire_request_bodies(CAP)
    wire_has_marker = any(MARK in json.dumps(b) for b in bodies)
    wire_has_error = any('"is_error": true' in json.dumps(b).lower() for b in bodies)

    print("==== FAILURE-COMPLETION SIGNAL PROBE ====")
    print(f"events seen: {[(e['event'], e.get('tool')) for e in rows]}")
    print(f"PostToolUseFailure fired (the failure hook): {bool(fail)}")
    print(f"PostToolUse fired (should be False — success-only): {bool(succ)}")
    print(f"failure hook payload carries stderr marker: {hook_has_marker}")
    if fail:
        print(f"\nPostToolUseFailure payload:\n{json.dumps(fail_payload, indent=2)[:1100]}")
    print(f"\n[wire backup] tool_result with stderr marker in a later request: {wire_has_marker}")
    print(f"[wire backup] tool_result flagged is_error: {wire_has_error}")

    if fail and not succ:
        verdict = ("CONFIRMED — a failed tool fires PostToolUseFailure (NOT PostToolUse); "
                   "AO must register both to capture every completion.")
    elif not fail and not succ and wire_has_marker:
        verdict = ("PostToolUseFailure did NOT fire on this binary — failed-tool completion "
                   "is WIRE-ONLY (tool_result is_error in the next request); AO reads the wire.")
    elif succ:
        verdict = "UNEXPECTED — PostToolUse fired for a non-zero exit; re-examine."
    else:
        verdict = "INCONCLUSIVE — inspect above."
    print(f"\nVERDICT: {verdict}")
    print("pty log:", PTY_LOG)
    print("=========================================")


if __name__ == "__main__":
    main()
