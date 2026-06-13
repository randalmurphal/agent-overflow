#!/usr/bin/env python3
"""Validate the PRODUCTION TUI launch posture the AO provider will actually use,
which no existing probe covered (they all used config-dir settings + default
mode). Three questions in one run:

  1. Clean full-access launch? Launch with the SAME flags AO's headless provider
     uses — `--permission-mode bypassPermissions --allow-dangerously-skip-permissions`
     — and check the one-time "Bypass Permissions mode" ACCEPTANCE Select
     (default row "No, exit") does NOT appear. (The earlier compaction probe hit
     it with the bare `--dangerously-skip-permissions`; this tests the production
     combo.)
  2. Flag-injected hook? Inject the lone required hook via the `--settings` FLAG
     (flagSettings layer) with config-dir settings.json left EMPTY — so any hook
     payload at all proves flag-injection works. This is what lets AO use the
     user's REAL config dir (native trust/auth) and just add the one hook,
     instead of managing an isolated config dir.
  3. AskUserQuestion answer-back via that flag-injected hook, 0 keystrokes — the
     one control op that genuinely needs a hook in full-access mode.

Safe: isolated config dir + pre-trusted /tmp cwd + a benign AskUserQuestion
prompt (no edits). Run via the inline runner (starts proxy, sets AO_BASE_URL).
"""
import json
import os
import re

import aoprobe

PTY_LOG = f"{aoprobe.AOHOOK}/pty-launchposture.log"
PROMPT = ("Use the AskUserQuestion tool to ask me to choose a filename: "
          "option A is 'alpha.txt', option B is 'beta.txt'. Ask exactly one "
          "question, then once I answer, just tell me which I picked and stop.")

# production full-access flags (mirrors options.go PermissionFlags) + flag-layer hook
PROD_FLAGS = ["--permission-mode", "bypassPermissions",
              "--allow-dangerously-skip-permissions"]


def saw_ask():
    return any(e.get("tool") == "AskUserQuestion"
               for e in aoprobe.payloads() if e.get("event") == "PreToolUse")


def deansi(path):
    try:
        data = open(path, "rb").read()
    except OSError:
        return ""
    data = re.sub(rb"\x1b\][0-9][^\x07]*\x07", b"", data)
    data = re.sub(rb"\x1b\[[0-9;?]*[a-zA-Z]", b"", data)
    return bytes(c for c in data if 0x20 <= c < 0x7f).replace(b" ", b"").lower().decode()


def main():
    base = os.environ["AO_BASE_URL"]
    # CTL is seeded (answer_questions on) but config-dir settings.json gets NO
    # hooks — the only hook source is the --settings flag below.
    aoprobe.seed_config(events=[], answer_questions=True)
    flag_settings = {"hooks": {ev: [{"hooks": [{"type": "command",
                     "command": f"python3 {aoprobe.HOOK}"}]}]
                     for ev in aoprobe.ALL_EVENTS}}

    sess = aoprobe.ClaudeSession(
        PROMPT, base, PTY_LOG,
        extra_args=[*PROD_FLAGS, "--settings", json.dumps(flag_settings)])
    sess.start()
    sess.run(until=saw_ask, max_s=150)
    sess._drain(8.0)
    ks = sess.keystrokes
    sess.exit()

    rows = aoprobe.payloads()
    events = [(e.get("event"), e.get("tool")) for e in rows]
    flag_hook_fired = len(rows) > 0          # only source is the --settings flag
    ask_pre = [e for e in rows if e["event"] == "PreToolUse"
               and e.get("tool") == "AskUserQuestion"]

    # bypass-acceptance Select present in the PTY?
    pty = deansi(PTY_LOG)
    ack_shown = ("bypasspermissionsmode" in pty and
                 ("yesiaccept" in pty or "acceptallresponsibility" in pty
                  or "no,exit" in pty.replace(",", "") or "noexit" in pty))

    # answer landed as a tool_result?
    tpath = next((e["payload"].get("transcript_path") for e in rows
                  if e["payload"].get("transcript_path")), None)
    answered, ctx = False, ""
    if tpath and os.path.exists(tpath):
        for ln in open(tpath, errors="replace"):
            try:
                m = json.loads(ln)
            except (json.JSONDecodeError, ValueError):
                continue
            content = (m.get("message") or {}).get("content")
            if isinstance(content, list):
                for b in content:
                    if b.get("type") == "tool_result":
                        c = b.get("content")
                        cs = c if isinstance(c, str) else json.dumps(c)
                        if any(w in cs.lower() for w in
                               ("answered your questions", "alpha", "beta")):
                            answered, ctx = True, cs[:200]

    print("==== PRODUCTION TUI LAUNCH-POSTURE PROBE ====")
    print("flags:", " ".join(PROD_FLAGS), "+ --settings <flag-layer hook>")
    print("timeline:", events)
    print()
    print(f"[Q1] bypass-acceptance Select shown?   {ack_shown}  "
          f"-> clean full-access launch = {not ack_shown}")
    print(f"[Q2] flag-injected hook fired?         {flag_hook_fired}  "
          f"(config-dir settings.json had NO hooks; only --settings did)")
    print(f"[Q3] AskUserQuestion answered via hook: {answered}  "
          f"keystrokes_during_turn={ks} (must be 0)")
    print(f"     tool_result ctx: {ctx!r}")
    print()
    verdict = ("CONFIRMED: clean full-access launch + flag-injected hook + "
               "AskUserQuestion answer-back, 0 keystrokes"
               if (not ack_shown and flag_hook_fired and answered and ks == 0)
               else "PARTIAL/NO — read the per-Q lines + pty log above")
    print("VERDICT:", verdict)
    print("pty log:", PTY_LOG)
    print("=============================================")


if __name__ == "__main__":
    main()
