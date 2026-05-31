#!/usr/bin/env python3
"""Probe: can the hook answer a MULTI-question AskUserQuestion (2-4 questions in
one call) via updatedInput.answers — keyed by question TEXT — with 0 keystrokes,
and does Claude match every answer (no re-prompt for any question)?

Open item #1 in the coverage map: single-question answer-via-hook is LIVE; the
schema allows 2-4 questions and the relay already builds an answers-map per
question, but only the single case was confirmed. This closes it.

The relay (answer_questions mode) echoes tool_input back INTACT and adds
`answers` = {question_text -> first option label} for EACH question. If Claude
matched by position instead of text, a text-keyed map would silently half-work,
so we assert: (a) the model asked >=2 questions in one call, (b) the tool_result
reports the answers and the model's follow-up names the FIRST-option label of
EVERY question, (c) 0 keystrokes and no native re-prompt fired.
"""
import json
import os
import re

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
# Discriminator: when set, the relay inserts the answers map in REVERSE question
# order. Text-keyed matching (lookup by question string) is unaffected; positional
# matching (Nth answer -> Nth question) would misassign. Run once with this set to
# PROVE the match is text-keyed — which is what AO relies on, since AO's own UI may
# return answers in a different order than the `questions` array.
REVERSE = bool(os.environ.get("AO_MULTIQ_REVERSE"))
PTY_LOG = f"{aoprobe.AOHOOK}/pty-multiq.log"
PROMPT = (
    "Use the AskUserQuestion tool ONCE to ask me these three things in a single "
    "call (all three questions in the one tool call):\n"
    "1. Preferred language? options: Python, Go\n"
    "2. Preferred test framework? options: pytest, gotest\n"
    "3. Preferred license? options: MIT, Apache-2.0\n"
    "After I answer, reply with exactly: PICKED <lang> / <framework> / <license> "
    "filling in my three choices, then stop."
)


def saw_ask():
    return any(e.get("tool") == "AskUserQuestion"
               for e in aoprobe.payloads() if e.get("event") == "PreToolUse")


def main():
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, answer_questions=True)
    if REVERSE:
        aoprobe.set_ctl(reverse_answers=True)
    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()
    sess.run(until=saw_ask, max_s=150)
    sess._drain(8.0)
    ks = sess.keystrokes               # MUST be 0
    saw_prompt = sess.saw_tui_perm     # MUST be False (no native re-prompt)
    sess.exit()

    rows = aoprobe.payloads()
    ask_pre = [e for e in rows if e["event"] == "PreToolUse"
               and e.get("tool") == "AskUserQuestion"]
    questions = (ask_pre[0]["payload"].get("tool_input", {}).get("questions")
                 if ask_pre else []) or []
    # The relay answers each question with its FIRST option's label.
    expected = []
    for q in questions:
        opts = q.get("options") or []
        if opts:
            lab = opts[0].get("label") if isinstance(opts[0], dict) else opts[0]
            expected.append(str(lab))

    tpath = next((e["payload"].get("transcript_path") for e in rows
                  if e["payload"].get("transcript_path")), None)
    answered_result = False
    answer_text = ""
    follow_up = ""
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
                        if ("have been answered" in cs.lower()
                                or "answered your questions" in cs.lower()):
                            answered_result = True
                            answer_text = cs   # the per-question Q=A mapping
                    if b.get("type") == "text" and msg.get("role") == "assistant":
                        follow_up = b.get("text", "")

    # Every question's chosen (first-option) label must appear in the model's
    # follow-up — proof the text-keyed answers reached the model intact.
    all_matched = bool(expected) and all(lab in follow_up for lab in expected)

    # Authoritative per-question check, read from the tool_result Q=A mapping (not
    # the model's paraphrase): each question must map to ITS OWN first option.
    # In REVERSE mode the relay built `answers` in reverse question order, so this
    # passing PROVES the match is text-keyed (positional would misassign here).
    expect_map = {}
    for q in questions:
        opts = q.get("options") or []
        if opts:
            lab = opts[0].get("label") if isinstance(opts[0], dict) else opts[0]
            expect_map[q.get("question", "")] = str(lab)
    got_map = dict(re.findall(r'"([^"]+\?)"="([^"]+)"', answer_text))
    per_q_ok = bool(expect_map) and all(got_map.get(q) == a for q, a in expect_map.items())

    print("==== MULTI-QUESTION ASKUSERQUESTION PROBE ====")
    print(f"answer-order mode: {'REVERSE (text-vs-positional discriminator)' if REVERSE else 'normal'}")
    print(f"AskUserQuestion calls: {len(ask_pre)}   questions in first call: {len(questions)}  (need >=2)")
    for q in questions:
        opts = [(o.get("label") if isinstance(o, dict) else o) for o in (q.get("options") or [])]
        print(f"   - {q.get('question')!r}  options={opts}")
    print(f"expected (first-option) answers: {expected}")
    print(f"tool_result = 'answered your questions': {answered_result}")
    print(f"tool_result Q=A mapping: {got_map}")
    print(f"expected Q->first-option mapping: {expect_map}")
    print(f"every question mapped to ITS OWN answer (text-keyed, not positional): {per_q_ok}")
    print(f"model follow-up: {follow_up!r}")
    print(f"every answer present in follow-up: {all_matched}")
    print(f"keystrokes during turn (MUST be 0): {ks}")
    print(f"native re-prompt fired (MUST be False): {saw_prompt}")
    if REVERSE:
        print("  [reverse mode] relay inserted answers in REVERSE question order; "
              "per-question correctness above PROVES text-keyed matching.")
    ok = (len(questions) >= 2 and answered_result and all_matched and per_q_ok
          and ks == 0 and not saw_prompt)
    print(f"\nVERDICT: {'CONFIRMED multi-question answer-via-hook' if ok else 'NOT CONFIRMED — inspect'}")
    print("pty log:", PTY_LOG)
    print("==============================================")


if __name__ == "__main__":
    main()
