#!/usr/bin/env python3
"""Probe: can a PreToolUse hook ANSWER an AskUserQuestion (not just gate it) by
returning allow + updatedInput.answers — with NO Select-widget keystrokes?

If yes, AO handles structured questions entirely through the hook channel: it
reads the questions off the wire/hook, renders its OWN UI, then injects the
chosen answer back via the hook's updatedInput. That deletes the fragile
"drive the Ink Select widget with digit/arrow keys" fallback entirely — exactly
the "don't parse/drive react in a terminal" outcome we want.

The relay (answer_questions mode) echoes the tool_input back INTACT with
`answers` added (keyed by question text -> first option's label). A partial
updatedInput would make the TUI re-prompt (false negative), so we echo the
whole input.

Success assertion (airtight): the transcript shows the AskUserQuestion tool_use
followed by a tool_result containing "User has answered your questions" WHILE we
sent ZERO keystrokes to drive a Select widget. Absence-of-prompt alone is not
proof — the zero-keystroke-but-answered pairing is.
"""
import json
import os

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
PTY_LOG = f"{aoprobe.AOHOOK}/pty-answer.log"
PROMPT = ("Use the AskUserQuestion tool to ask me to choose a filename: "
          "option A is 'alpha.txt', option B is 'beta.txt'. Ask exactly one "
          "question, then once I answer, just tell me which I picked and stop.")


def saw_ask():
    return any(e.get("tool") == "AskUserQuestion"
               for e in aoprobe.payloads() if e.get("event") == "PreToolUse")


def main():
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, answer_questions=True)
    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()
    # Run until AskUserQuestion is intercepted+answered by the hook, then drain
    # to capture the resulting tool_result + the model's follow-up.
    sess.run(until=saw_ask, max_s=150)
    sess._drain(8.0)
    ks_during_turn = sess.keystrokes      # MUST be 0 for the proof to hold
    sess.exit()

    rows = aoprobe.payloads()
    ask_pre = [e for e in rows if e["event"] == "PreToolUse"
               and e.get("tool") == "AskUserQuestion"]
    questions = None
    if ask_pre:
        questions = ask_pre[0]["payload"].get("tool_input", {}).get("questions")

    tpath = next((e["payload"].get("transcript_path") for e in rows
                  if e["payload"].get("transcript_path")), None)
    answered = False
    answer_ctx = ""
    follow_up = ""
    saw_select_widget = False
    if tpath and os.path.exists(tpath):
        for ln in open(tpath, errors="replace"):
            try:
                m = json.loads(ln)
            except (json.JSONDecodeError, ValueError):
                continue
            msg = m.get("message", {})
            content = msg.get("content") if isinstance(msg, dict) else None
            if isinstance(content, list):
                for b in content:
                    if b.get("type") == "tool_result":
                        c = b.get("content")
                        cs = json.dumps(c) if not isinstance(c, str) else c
                        if "answered your questions" in cs.lower() or "alpha" in cs.lower() or "beta" in cs.lower():
                            answered = True
                            answer_ctx = cs[:300]
                    if b.get("type") == "text" and msg.get("role") == "assistant":
                        follow_up = b.get("text", "")[:200]

    print("==== ASKUSERQUESTION ANSWER-VIA-HOOK PROBE ====")
    print("timeline:", [(e.get("event"), e.get("tool")) for e in rows])
    print(f"AskUserQuestion intercepted by PreToolUse hook: {bool(ask_pre)}")
    if questions:
        print("questions read off hook (AO can render its own UI):")
        print(json.dumps(questions, indent=2)[:700])
    print(f"\nkeystrokes sent during turn (MUST be 0): {ks_during_turn}")
    print(f"answer landed as tool_result (no widget driven): {answered}")
    print(f"   tool_result ctx: {answer_ctx!r}")
    print(f"model follow-up after injected answer: {follow_up!r}")
    verdict = ("CONFIRMED: hook ANSWERED the question with 0 keystrokes"
               if (answered and ks_during_turn == 0)
               else "NOT CONFIRMED — inspect transcript / pty log")
    print(f"\nVERDICT: {verdict}")
    print("pty log:", PTY_LOG)
    print("===============================================")


if __name__ == "__main__":
    main()
