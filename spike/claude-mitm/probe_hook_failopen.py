#!/usr/bin/env python3
"""Probe: when a PreToolUse gate hook is KILLED at its timeout, does the tool
EXECUTE unreviewed, or does control FALL THROUGH to Claude's normal permission
flow (which, for an approval-requiring command, shows the native TUI prompt)?

This is the single most load-bearing design question for AO. The earlier
`probe_hook_timeout.py` could not answer it: it tested `echo > file`, and the
isolated config silently carried the user's auto-accept opt-in (the real
`~/.claude.json` ships `teammateMode`/auto-permission state), so EVERY command —
even `rm` — auto-ran with no prompt. "killed hook + tool ran" was therefore
consistent with BOTH mechanisms and proved nothing.

We remove every confound by making the gate DETERMINISTIC and explicit, not
heuristic:
  - settings.json `permissions.defaultMode = "default"` wipes out the carried
    auto-accept posture, and
  - settings.json `permissions.ask = ["Bash(rm:*)"]` forces any `rm ...` to PROMPT
    in normal flow regardless of mode (an explicit `ask` rule is ground truth for
    "needs approval"; it does not depend on guessing Claude's risk classifier).

Four cases, everything held constant except the one variable under test:

  1. baseline_nohook_echo  (no hook, default mode, NO ask rule, echo>file)
       Shows what TRUE default mode does for a plain file write with no auto
       opt-in and no rule — isolates whether the old auto-run was the opt-in.
  2. control_nohook_rm      (no hook, default mode, ask rm, rm file)
       MUST show a native prompt + rm intact. Validates that the ask rule really
       gates rm with no hook in the loop. If this does not prompt, the harness
       cannot force a prompt and cases 3/4 are not trustworthy — so this case is
       the litmus the decider rests on.
  3. decider_killed_rm      (PreToolUse deny, sleep 6, timeout 2 -> KILLED; ask rm)
       THE DECIDER:
         rm ran (file gone, PostToolUse)  -> FAIL-OPEN-TO-EXECUTE (the killed hook
            bypassed normal flow entirely, ignoring an explicit ask rule)
         native prompt, rm intact         -> FALL-THROUGH-TO-PROMPT (control
            reverts to normal flow incl. the ask rule; AO's non-TUI channel cannot
            answer it -> the real failure mode is degrade-to-STUCK)
         neither                          -> FAIL-CLOSED
  4. sanity_survive_rm      (PreToolUse deny, sleep 1, timeout 10 -> SURVIVES; ask rm)
       Hook returns deny before its timeout -> rm blocked, FINISHED marker, no
       prompt. Confirms the surviving-hook path is unaffected (and that a returned
       deny outranks the ask rule).

Both decider outcomes still imply the same AO rule (the gate hook must own its
deadline and always emit an explicit decision), but they mean OPPOSITE failure
UX, so AO's fallback design depends on which is real. Detection is filesystem
side-effects + the relay FINISHED marker + the harness native-prompt detector
(corroborated by a broad PTY scan). The submit-nudge is disabled so a stray
Enter can never auto-accept a native prompt.
"""
import os
import re
import time

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
ECHO_OUT = f"{aoprobe.AOHOOK}/fo_echo.txt"
RM_TARGET = f"{aoprobe.AOHOOK}/fo_rm.txt"
ASK_RM = ["Bash(rm:*)"]

# name, kind, has_gate_hook, decision, sleep_s, timeout_s, ask_rules
CASES = [
    ("baseline_nohook_echo", "echo", False, "allow", 0.0, None, None),
    ("control_nohook_rm",    "rm",   False, "allow", 0.0, None, ASK_RM),
    ("decider_killed_rm",    "rm",   True,  "deny",  6.0, 2,    ASK_RM),
    ("sanity_survive_rm",    "rm",   True,  "deny",  6.0, 12,   ASK_RM),
]

# Permission-prompt evidence in raw PTY bytes (corroborates saw_tui_perm). Only
# strong prompt-BOX signals — NOT "esc to interrupt" (shown every working turn)
# and NOT the word "permission" (appears in help text). False positives here
# would mask a real "tool ran without a prompt" result.
PROMPT_PAT = re.compile(
    rb"do you want to|wants to run|\xe2\x9d\xaf\s*\d+\.\s|\b\d+\.\s*yes\b", re.I)


def cmd_for(kind):
    return (f"echo PROBE-OK > {ECHO_OUT}" if kind == "echo"
            else f"rm {RM_TARGET}")


def prep_side_effect(kind):
    """echo: target ABSENT (presence => ran). rm: target PRESENT (absence => ran)."""
    if kind == "echo":
        try:
            os.remove(ECHO_OUT)
        except OSError:
            pass
    else:
        with open(RM_TARGET, "w") as f:
            f.write("delete-me\n")


def ran(kind):
    return os.path.exists(ECHO_OUT) if kind == "echo" else not os.path.exists(RM_TARGET)


def pty_prompt_seen(log_path):
    try:
        return bool(PROMPT_PAT.search(open(log_path, "rb").read()))
    except OSError:
        return False


def run_case(name, kind, has_gate_hook, decision, sleep_s, timeout_s, ask_rules):
    events = (["PreToolUse", "PostToolUse"] if has_gate_hook else ["PostToolUse"])
    aoprobe.seed_config(events=events, decision=decision, sleep_s=sleep_s,
                        timeout_s=timeout_s, default_mode="default",
                        ask_rules=ask_rules)
    prep_side_effect(kind)
    prompt = ("Use the Bash tool to run exactly this command and nothing else: "
              + cmd_for(kind))
    log = f"{aoprobe.AOHOOK}/pty-fo-{name}.log"
    sess = aoprobe.ClaudeSession(prompt, BASE_URL, log)
    sess.start()
    # Disable the submit-nudge for the whole run (no_hook_probe=lambda:False) so a
    # stray Enter can never auto-accept a native permission prompt. The positional
    # prompt auto-submits on its own.
    sess.run(until=lambda: ran(kind) or sess.saw_tui_perm,
             max_s=35 if has_gate_hook else 30,
             no_hook_probe=lambda: False)
    sess._drain(4.0)
    sess.exit()

    rows = aoprobe.payloads()
    pre = [e for e in rows if e["event"] == "PreToolUse" and e.get("tool") == "Bash"]
    post = [e for e in rows if e["event"] == "PostToolUse" and e.get("tool") == "Bash"]
    fin = [e for e in rows if e["event"] == "PreToolUse-FINISHED"]
    held = round(fin[0].get("since_entry"), 1) if fin and fin[0].get("since_entry") else None
    return {
        "case": name,
        "kind": kind,
        "gate_hook": has_gate_hook,
        "pre_fired": len(pre) > 0,
        "post_fired": len(post) > 0,
        "hook_survived (FINISHED)": bool(fin),
        "held_s (hook held tool open)": held,
        "tool_ran": ran(kind),
        "prompt (detector)": sess.saw_tui_perm,
        "prompt (pty scan)": pty_prompt_seen(log),
        "log": log,
    }


def interpret(by):
    out = []
    base = by.get("baseline_nohook_echo", {})
    ctl = by.get("control_nohook_rm", {})
    dec = by.get("decider_killed_rm", {})
    san = by.get("sanity_survive_rm", {})

    if base.get("tool_ran") and not base.get("prompt (detector)"):
        out.append("baseline: even under defaultMode=default, `echo>file` auto-runs "
                   "(default mode does not gate a plain write here).")
    elif base.get("prompt (detector)") or base.get("prompt (pty scan)"):
        out.append("baseline: under defaultMode=default, `echo>file` PROMPTS -> the "
                   "earlier auto-run was the carried auto-accept opt-in.")
    else:
        out.append("baseline: inconclusive (echo neither ran nor prompted).")

    ctl_prompted = ctl.get("prompt (detector)") or ctl.get("prompt (pty scan)")
    if ctl_prompted and not ctl.get("tool_ran"):
        out.append("control: ask rule GATES rm with no hook (native prompt, not run) "
                   "-> the harness can force a prompt; the decider is trustworthy.")
        litmus_ok = True
    else:
        out.append("control: ask rule did NOT gate rm (ran=%s prompt=%s) -> CANNOT "
                   "force a prompt; decider is NOT trustworthy, investigate."
                   % (ctl.get("tool_ran"), ctl_prompted))
        litmus_ok = False

    if san.get("hook_survived (FINISHED)") and not san.get("tool_ran") \
            and not (san.get("prompt (detector)") or san.get("prompt (pty scan)")):
        out.append("sanity: a SURVIVING deny hook blocks rm (no run, no prompt) "
                   "-> returned deny outranks the ask rule; hook path intact.")
    else:
        out.append("sanity: unexpected (survived=%s ran=%s prompt=%s)."
                   % (san.get("hook_survived (FINISHED)"), san.get("tool_ran"),
                      san.get("prompt (detector)") or san.get("prompt (pty scan)")))

    dec_prompted = dec.get("prompt (detector)") or dec.get("prompt (pty scan)")
    if dec.get("tool_ran"):
        verdict = ("FAIL-OPEN-TO-EXECUTE: a killed gate hook let an approval-gated "
                   "command RUN, bypassing an explicit ask rule.")
    elif dec_prompted:
        verdict = ("FALL-THROUGH-TO-PROMPT: a killed gate hook reverts to normal "
                   "flow (ask rule -> native prompt). AO cannot answer it over its "
                   "non-TUI channel -> degrade-to-STUCK.")
    else:
        verdict = "FAIL-CLOSED: a killed gate hook blocked the tool (no run, no prompt)."
    out.append("DECIDER: " + verdict
               + ("" if litmus_ok else "  [WARNING: control litmus failed]"))
    return out, (litmus_ok, verdict)


def main():
    by = {}
    for spec in CASES:
        name = spec[0]
        print(f"\n[probe] === {name} ===")
        r = run_case(*spec)
        by[name] = r
        for k, v in r.items():
            print(f"    {k}: {v}")
        time.sleep(1.0)

    print("\n==== FAIL-OPEN MATRIX (defaultMode=default; rm gated by ask rule) ====")
    print(f"  {'case':<22}{'gate':<6}{'ran':<7}{'prompt':<8}{'post':<6}{'FINISHED':<9}")
    for spec in CASES:
        r = by[spec[0]]
        prompt = r["prompt (detector)"] or r["prompt (pty scan)"]
        print(f"  {spec[0]:<22}{str(r['gate_hook']):<6}{str(r['tool_ran']):<7}"
              f"{str(prompt):<8}{str(r['post_fired']):<6}"
              f"{str(r['hook_survived (FINISHED)']):<9}")
    print("\n==== INTERPRETATION ====")
    lines, _ = interpret(by)
    for ln in lines:
        print("  " + ln)
    print("=========================================================")


if __name__ == "__main__":
    main()
