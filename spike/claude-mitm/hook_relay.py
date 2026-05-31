#!/usr/bin/env python3
"""PreToolUse (and any) hook relay used by the hook-channel probes.

Claude Code runs this as a hook command. It:
  1. reads the hook payload JSON from stdin,
  2. appends {ts, event, raw} to AO_HOOK_LOG (so the probe can inspect the
     exact payload shape Claude delivers — the thing AO's real relay needs),
  3. optionally sleeps AO_HOOK_SLEEP seconds (to test that Claude blocks while
     a hook deliberates — i.e. while AO would be rendering an approval to a human),
  4. emits a decision on stdout for PreToolUse (allow/deny), read from a control
     file so the driver can flip it per run without re-launching.

stdout MUST be only the JSON response — all logging goes to files/stderr.

Control file (AO_HOOK_CTL, default /tmp/aohook/ctl.json):
  {"decision": "allow"|"deny", "sleep": <seconds>, "schema": "modern"|"legacy",
   "reason": "..."}
"""
import json
import os
import sys
import time

LOG = os.environ.get("AO_HOOK_LOG", "/tmp/aohook/payloads.jsonl")
CTL = os.environ.get("AO_HOOK_CTL", "/tmp/aohook/ctl.json")


def load_ctl():
    try:
        with open(CTL) as f:
            return json.load(f)
    except (OSError, json.JSONDecodeError):
        return {}


def main():
    raw = sys.stdin.read()
    ts = time.time()
    try:
        payload = json.loads(raw)
    except (json.JSONDecodeError, ValueError):
        payload = None

    event = (payload or {}).get("hook_event_name") or (payload or {}).get("hookEventName") or "?"
    tool = (payload or {}).get("tool_name") or (payload or {}).get("toolName") or ""

    os.makedirs(os.path.dirname(LOG), exist_ok=True)
    with open(LOG, "a") as f:
        f.write(json.dumps({"ts": ts, "event": event, "tool": tool, "raw": raw}) + "\n")

    ctl = load_ctl()
    sleep_s = float(ctl.get("sleep", os.environ.get("AO_HOOK_SLEEP", 0)) or 0)
    if sleep_s > 0:
        time.sleep(sleep_s)
        # Finish-marker: only reached if the hook was NOT killed at its timeout.
        # Lets the timeout probe distinguish "killed -> fell through" from
        # "survived -> returned its decision". Distinct event name so the other
        # probes' tool-name filters ignore it.
        with open(LOG, "a") as f:
            f.write(json.dumps({"ts": time.time(), "event": f"{event}-FINISHED",
                                "tool": tool, "raw": "",
                                "slept": sleep_s, "since_entry": time.time() - ts}) + "\n")

    # Only PreToolUse needs a gating decision. For every other event, a clean
    # exit 0 with no stdout is "observe, don't interfere".
    if event != "PreToolUse":
        return 0

    decision = ctl.get("decision", "allow")
    reason = ctl.get("reason", f"AO probe decision={decision}")
    schema = ctl.get("schema", "modern")

    # Answer mode: for AskUserQuestion, return allow + updatedInput that echoes
    # the tool_input back INTACT with `answers` added (keyed by question text ->
    # first option label). This tests whether a hook can ANSWER the question
    # (no Select widget) vs. merely gate it. The full input must be echoed; a
    # partial updatedInput would make the TUI re-prompt (false negative).
    if ctl.get("answer_questions") and tool == "AskUserQuestion":
        tool_input = (payload or {}).get("tool_input") or {}
        qs = list(tool_input.get("questions", []))
        if ctl.get("reverse_answers"):
            # Insert the answers map in REVERSE question order. A text-keyed
            # consumer (lookup by question string) is unaffected; a positional
            # consumer (Nth answer -> Nth question) would misassign. This is the
            # discriminator between the two.
            qs = list(reversed(qs))
        answers = {}
        for q in qs:
            opts = q.get("options") or []
            label = (opts[0].get("label") if opts and isinstance(opts[0], dict)
                     else (opts[0] if opts else "yes"))
            answers[q.get("question", "")] = label
        updated = dict(tool_input)
        updated["answers"] = answers
        out = {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "allow",
                "permissionDecisionReason": "AO probe answered via hook",
                "updatedInput": updated,
            }
        }
        sys.stdout.write(json.dumps(out))
        sys.stdout.flush()
        return 0

    if schema == "legacy":
        out = {"decision": "approve" if decision == "allow" else "block", "reason": reason}
    else:
        out = {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": decision,  # allow | deny | ask
                "permissionDecisionReason": reason,
            }
        }
    sys.stdout.write(json.dumps(out))
    sys.stdout.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
