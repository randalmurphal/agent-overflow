#!/usr/bin/env python3
"""Probe: are edits to DANGEROUS_FILES/DANGEROUS_DIRECTORIES *bypass-immune* —
i.e. does a PreToolUse hook that returns `allow` STILL get overridden by a
native permission dialog for sensitive paths (.zshrc / .mcp.json / .git/ /
.claude/ ...)?

Why it matters: AO's whole "the relay IS the permission posture" model assumes a
hook `allow` suppresses the native TUI prompt (validated for ordinary tools:
HOOKS_COVERAGE_MAP.md "allow suppresses the TUI prompt entirely"). The v2.1.88
source says there is ONE documented exception — utils/permissions/permissions.ts
keeps a `safetyCheck` ask "even when a PreToolUse hook returned allow", and
utils/permissions/filesystem.ts defines the bounded set:
  DANGEROUS_FILES       = .gitconfig .gitmodules .bashrc .bash_profile .zshrc
                          .zprofile .profile .ripgreprc .mcp.json .claude.json
  DANGEROUS_DIRECTORIES = .git .vscode .idea .claude   (basename / path-segment)
This confirms it on the 2.1.158 BINARY and shows what AO observes when it fires.

Design: relay decision=allow for everything. ONE session, two Write calls:
  (control) notes.txt -> expect PreToolUse(allow) -> PostToolUse(ran) -> written
  (test)    .zshrc    -> expect PreToolUse(allow) -> NO PostToolUse; a native
            dialog appears (hook-allow overridden); file NOT written until answered.
The within-session contrast rules out config drift (same hook, same posture). We
never answer the dialog — teardown sends Esc (aoprobe.exit), so nothing sensitive
is written. zero TUI driving (keystrokes stays at the trust \r + teardown esc).
"""
import os
import time
import json

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-dangerpath.log"
CONTROL = os.path.join(aoprobe.CWD, "notes.txt")
DANGER = os.path.join(aoprobe.CWD, ".zshrc")

PROMPT = (
    "No preamble, no explanation. Use the Write tool for each step, one tool "
    "call at a time, in this order:\n"
    "1. Create a file named notes.txt in the current directory with exactly "
    "this content: hello\n"
    "2. Create a file named .zshrc in the current directory with exactly this "
    "content: export AO_TEST=1\n"
    "After both writes, reply with the single word DONE."
)

# The safetyCheck dialog renders "Claude requested permissions to edit <p> which
# is a sensitive file." De-ANSI'd + space-stripped + lowercased markers. We OR a
# few candidates so a reworded dialog still trips, but the decisive signal is the
# STALL (allowed-but-didn't-run + file-absent), which is phrasing-independent.
DIALOG_MARKERS = (b"sensitivefile", b"requestedpermission", b"haventgranted",
                  b"doyouwant", b"wantsto")


def _has_dialog(sess):
    norm = aoprobe._norm(sess._rawtail)
    return any(m in norm for m in DIALOG_MARKERS)


def _writes(rows, ev):
    return [e for e in rows if e.get("event") == ev and e.get("tool") == "Write"]


def _path_of(e):
    ti = e["payload"].get("tool_input") or {}
    return ti.get("file_path", "") or ti.get("path", "") or json.dumps(ti)


def main():
    aoprobe.seed_config(
        events=["PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop"],
        decision="allow")
    for f in (CONTROL, DANGER):
        try:
            os.remove(f)
        except OSError:
            pass

    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()

    danger_pre_at = None
    dialog_seen = False
    while sess.elapsed() < 150:
        sess._pump_once(no_hook_yet=(not aoprobe.payloads()))
        rows = aoprobe.payloads()
        if any(".zshrc" in _path_of(e) for e in _writes(rows, "PreToolUse")) \
                and danger_pre_at is None:
            danger_pre_at = time.time()
        if _has_dialog(sess):
            dialog_seen = True
        dang_post = any(".zshrc" in _path_of(e) for e in _writes(rows, "PostToolUse"))
        if dialog_seen or dang_post:
            break
        if danger_pre_at and time.time() - danger_pre_at > 20:
            break  # decisive stall: hook allowed the write, yet it never ran
    sess._drain(2.0)

    # snapshot before teardown (exit() sends Esc, which dismisses the dialog)
    saw_tui = sess.saw_tui_perm or dialog_seen
    keystrokes = sess.keystrokes
    norm_log = ""
    if os.path.exists(PTY_LOG):
        norm_log = aoprobe._norm(open(PTY_LOG, "rb").read()).decode("ascii", "replace")
    sess.exit()

    rows = aoprobe.payloads()
    ctrl_pre = any("notes.txt" in _path_of(e) for e in _writes(rows, "PreToolUse"))
    ctrl_post = any("notes.txt" in _path_of(e) for e in _writes(rows, "PostToolUse"))
    dang_pre = any(".zshrc" in _path_of(e) for e in _writes(rows, "PreToolUse"))
    dang_post = any(".zshrc" in _path_of(e) for e in _writes(rows, "PostToolUse"))
    dang_postf = any(".zshrc" in _path_of(e) for e in rows
                     if e.get("event") == "PostToolUseFailure" and e.get("tool") == "Write")

    idx = -1
    for m in ("sensitivefile", "requestedpermission", "doyouwant", "wantsto"):
        idx = norm_log.find(m)
        if idx != -1:
            break
    ctx = norm_log[max(0, idx - 60):idx + 140] if idx != -1 else norm_log[-300:]

    print("==== DANGEROUS-PATH BYPASS-IMMUNITY (hook=allow) ====")
    print("target: 2.1.158 binary  |  source basis: v2.1.88 filesystem.ts")
    print(f"keystrokes driven into TUI (trust \\r + teardown esc only): {keystrokes}")
    print("\n-- CONTROL  Write notes.txt (benign) --")
    print(f"   PreToolUse(hook allow): {ctrl_pre}   PostToolUse(ran): {ctrl_post}"
          f"   file exists: {os.path.exists(CONTROL)}")
    print("\n-- TEST     Write .zshrc (DANGEROUS_FILES basename) --")
    print(f"   PreToolUse(hook allow): {dang_pre}   PostToolUse(ran): {dang_post}"
          f"   PostToolUseFailure: {dang_postf}")
    print(f"   native dialog detected: {saw_tui}")
    print(f"   .zshrc exists (False => held, not written): {os.path.exists(DANGER)}")
    print(f"   dialog ctx: {ctx!r}")

    print("\nVERDICT:")
    if ctrl_post and dang_pre and not dang_post and (saw_tui or not os.path.exists(DANGER)):
        print("  CONFIRMED bypass-immune on 2.1.158. The SAME hook=allow ran the benign")
        print("  write but the .zshrc write was held by a NATIVE dialog. AO's relay cannot")
        print("  suppress sensitive-path edits via the hook; it must drive/own that dialog")
        print("  or hard-deny. Set is bounded: DANGEROUS_FILES + DANGEROUS_DIRECTORIES.")
    elif ctrl_post and dang_post:
        print("  NOT bypass-immune on 2.1.158 — hook=allow ran the .zshrc write too.")
        print("  (Source v2.1.88 says it should gate; binary differs — investigate.)")
    else:
        print("  INCONCLUSIVE — inspect payloads + dialog ctx above.")
    print("pty log:", PTY_LOG)
    print("=====================================================")


if __name__ == "__main__":
    main()
