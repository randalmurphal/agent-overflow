#!/usr/bin/env python3
"""Probe: can AO DRIVE the bypass-immune native dialog via the PTY?

probe_hook_dangerpath.py proved a DANGEROUS_FILES edit is held by a native
numbered dialog even when the PreToolUse hook returns `allow`. That dialog is
NOT hook-suppressible (unlike AskUserQuestion, which is hook-answered with zero
keystrokes). So the open question for the "AO drives/owns the dialog" branch is:
if AO sends the digit for "1. yes" into the PTY, does the held edit actually
proceed (PostToolUse fires + file written with the right content)?

This validates the LAST unknown in that branch: deterministic digit-drive of a
native permission dialog. Decision is the user's (drive vs hard-deny); this only
establishes that driving is mechanically possible.

Setup: relay decision=allow, single .zshrc write into the throwaway cwd. When the
dialog renders, send "1" (select "yes"); fall back to Enter if the digit alone
doesn't advance. Success = PostToolUse(Write .zshrc) + file present with content.
keystrokes>0 here is the POINT (we are deliberately driving), unlike other probes.
"""
import os
import time
import json

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-dangerdrive.log"
DANGER = os.path.join(aoprobe.CWD, ".zshrc")
EXPECT = "export AO_TEST=1"

PROMPT = (
    "No preamble, no explanation. Use the Write tool to create a file named "
    ".zshrc in the current directory with exactly this content: " + EXPECT +
    "\nThen reply with the single word DONE."
)

DIALOG_MARKERS = (b"sensitivefile", b"requestedpermission", b"doyouwant",
                  b"wantsto", b"createfile.zshrc")


def _has_dialog(sess):
    norm = aoprobe._norm(sess._rawtail)
    return any(m in norm for m in DIALOG_MARKERS)


def _dang_post(rows):
    return any(e.get("event") == "PostToolUse" and e.get("tool") == "Write"
               and ".zshrc" in json.dumps(e["payload"].get("tool_input", {}))
               for e in rows)


def main():
    aoprobe.seed_config(
        events=["PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop"],
        decision="allow")
    try:
        os.remove(DANGER)
    except OSError:
        pass

    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()

    drove_digit = drove_enter = False
    digit_at = None
    while sess.elapsed() < 150:
        sess._pump_once(no_hook_yet=(not aoprobe.payloads()))
        rows = aoprobe.payloads()
        if _dang_post(rows) or os.path.exists(DANGER):
            break
        if _has_dialog(sess):
            if not drove_digit:
                sess.send("1")            # select "1. yes"
                drove_digit = True
                digit_at = time.time()
            elif (not drove_enter and digit_at and time.time() - digit_at > 4):
                sess.send("\r")           # fallback: confirm if digit didn't fire
                drove_enter = True
    sess._drain(2.0)

    rows = aoprobe.payloads()
    ran = _dang_post(rows)
    exists = os.path.exists(DANGER)
    content = ""
    if exists:
        try:
            content = open(DANGER).read().strip()
        except OSError:
            pass
    keystrokes = sess.keystrokes
    sess.exit()
    try:
        os.remove(DANGER)               # clean up the throwaway file
    except OSError:
        pass

    print("==== DRIVE THE BYPASS-IMMUNE DIALOG (hook=allow, PTY digit) ====")
    print(f"dialog rendered + driven: digit '1' sent={drove_digit}  enter-fallback={drove_enter}")
    print(f"keystrokes into TUI (drive is intentional here): {keystrokes}")
    print(f"PostToolUse(Write .zshrc) fired after drive: {ran}")
    print(f"file written: {exists}   content matches {EXPECT!r}: {content == EXPECT}  (got {content!r})")
    print("\nVERDICT:")
    if drove_digit and ran and content == EXPECT:
        print("  CONFIRMED — AO can DRIVE the native bypass-immune dialog via a PTY digit.")
        print("  The held edit proceeded after '1'. So the 'AO owns the dialog' branch is")
        print("  mechanically viable: detect the stall, route to the user, drive 1/3.")
    elif drove_digit and not ran:
        print("  DROVE the digit but the edit did NOT proceed — digit-drive insufficient")
        print("  (may need arrow-nav or different key). Inspect pty log.")
    else:
        print("  INCONCLUSIVE — dialog may not have rendered. Inspect pty log.")
    print("pty log:", PTY_LOG)
    print("================================================================")


if __name__ == "__main__":
    main()
