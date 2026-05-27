#!/usr/bin/env python3
"""Probe 3 — verify the plan-approval REJECT path AO will use.

Source says Esc on the approval Select = toolUseConfirm.onReject() with no
feedback, staying in plan mode (no setMode). That makes Esc the robust,
index-independent reject — important because the approve-dialog option indices
shift with showUltraplan/showClearContext, so blind `j`-counting to the reject
row is fragile.

We confirm on the binary:
  1. Get to the plan-approval dialog (bypass launch -> shift+tab to plan ->
     submit a plan task -> ExitPlanMode).
  2. Send a single Esc.
  3. Verify: dialog closes, footer STAYS "plan mode on", file NOT written,
     wire shows the ExitPlanMode tool rejected (no execution).
  4. Submit a refined prompt; verify the model re-plans (another ExitPlanMode)
     i.e. the session is fully usable after an Esc reject.
"""
import json
import os
import pty
import select
import struct
import fcntl
import termios
import sys
import time
import shutil
import re

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8093")
CAP = os.environ.get("AO_CAP_LOG", "/tmp/cap-planprobe.jsonl")
WORK = "/tmp/ao-planprobe3"
PTY_LOG = "/tmp/ao-planprobe3.log"
PLAN_PROMPT = "Add a file foo.txt containing HELLO to this directory."
REFINE_PROMPT = "Actually make it contain GOODBYE instead, and name it bar.txt."
HARD_TIMEOUT_S = 200
SHIFT_TAB = "\x1b[Z"


def clean(b):
    t = b.decode("utf-8", "replace") if isinstance(b, (bytes, bytearray)) else b
    t = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", t)
    t = re.sub(r"\x1b\][^\x07]*\x07", "", t)
    t = re.sub(r"\x1b[=>]", "", t)
    return "\n".join(l.rstrip() for l in t.splitlines() if l.strip())


def footer_mode(pty_log):
    try:
        full = open(pty_log, "rb").read()
    except OSError:
        return None
    tail = clean(full)[-2500:]
    norm = re.sub(r"\s+", "", tail.lower())
    hits = []
    for token, mode in [("bypasspermissionson", "bypass"), ("planmodeon", "plan"),
                        ("accepteditson", "acceptEdits"), ("automodeon", "auto")]:
        idx = 0
        while True:
            i = norm.find(token, idx)
            if i < 0:
                break
            hits.append((i, mode)); idx = i + 1
    if not hits:
        return "default"
    hits.sort()
    return hits[-1][1]


def dialog_present(pty_log):
    """True when the plan-approval Select is rendered (stable marker)."""
    try:
        tail = clean(open(pty_log, "rb").read())[-2500:]
    except OSError:
        return False
    norm = re.sub(r"\s+", "", tail.lower())
    return "readytocode" in norm and "tellclaudewhattochange" in norm


def set_winsize(fd, rows=50, cols=210):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def wire_tool_uses(cap_path, start_offset):
    try:
        with open(cap_path, "r", errors="replace") as f:
            f.seek(start_offset)
            lines = f.read().splitlines()
    except OSError:
        return []
    names = []
    for line in lines:
        try:
            e = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        if e.get("kind") != "response_chunk":
            continue
        for sse in e.get("text", "").splitlines():
            if not sse.startswith("data:"):
                continue
            try:
                d = json.loads(sse[5:].strip())
            except (json.JSONDecodeError, ValueError):
                continue
            if d.get("type") == "content_block_start":
                cb = d.get("content_block", {})
                if cb.get("type") == "tool_use":
                    names.append(cb.get("name"))
    return names


def cap_size(p):
    try:
        return os.path.getsize(p)
    except OSError:
        return 0


def main():
    if os.path.exists(WORK):
        shutil.rmtree(WORK)
    os.makedirs(WORK)
    env = dict(os.environ)
    env["ANTHROPIC_BASE_URL"] = BASE_URL
    env["TERM"] = "xterm-256color"
    os.chdir(WORK)
    foo = os.path.join(WORK, "foo.txt")
    bar = os.path.join(WORK, "bar.txt")

    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude", ["claude", "--dangerously-skip-permissions"], env)
        os._exit(127)
    set_winsize(master)

    logf = open(PTY_LOG, "wb")
    start = time.time()
    last_out = start
    result = {"events": []}
    phase = "boot"
    trust_done = False
    cycle_sends = 0
    next_at = 0
    plan_offset = None
    refine_offset = None

    def send(s):
        os.write(master, s.encode() if isinstance(s, str) else s)

    def note(m):
        msg = f"[p3 {time.time()-start:5.1f}s] {m}"
        print(msg, file=sys.stderr); result["events"].append(msg)

    while True:
        if time.time() - start > HARD_TIMEOUT_S:
            note("HARD TIMEOUT"); break
        r, _, _ = select.select([master], [], [], 0.25)
        now = time.time()
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            logf.write(data); logf.flush(); last_out = now
            low = data[-4000:].lower()
            if not trust_done and b"trust" in low and b"?" in low:
                note("trust -> Enter"); send("\r"); trust_done = True; next_at = now + 2.5
        quiet = now - last_out

        if phase == "boot":
            if (trust_done or now - start > 6) and quiet > 2.0 and now >= next_at:
                note(f"idle footer = {footer_mode(PTY_LOG)}"); phase = "cycle"; next_at = now + 0.3
        elif phase == "cycle":
            if now >= next_at:
                m = footer_mode(PTY_LOG)
                if m == "plan":
                    note(f"reached plan after {cycle_sends} shift+tab; submit plan prompt")
                    send(PLAN_PROMPT); time.sleep(0.4); send("\r")
                    plan_offset = cap_size(CAP); phase = "await_plan"; next_at = now + 1.0
                elif cycle_sends >= 7:
                    note(f"cycle stuck at {m}"); break
                else:
                    send(SHIFT_TAB); cycle_sends += 1; next_at = now + 0.7
        elif phase == "await_plan":
            names = wire_tool_uses(CAP, plan_offset)
            if any(n and "plan" in n.lower() for n in names) and dialog_present(PTY_LOG) and quiet > 1.5:
                note("approval dialog stable -> send single Esc (reject)")
                result["plan1_tooluses"] = names
                send("\x1b"); phase = "after_esc"; next_at = now + 3.0
            elif quiet > 60:
                note("no stable dialog in 60s quiet"); result["plan1_tooluses"] = names; break
        elif phase == "after_esc":
            if now >= next_at and quiet > 1.5:
                result["after_esc_mode"] = footer_mode(PTY_LOG)
                result["after_esc_dialog_present"] = dialog_present(PTY_LOG)
                result["after_esc_foo_exists"] = os.path.exists(foo)
                result["after_esc_screen"] = clean(open(PTY_LOG, "rb").read())[-1800:]
                note(f"after Esc: mode={result['after_esc_mode']} "
                     f"dialog={result['after_esc_dialog_present']} foo={result['after_esc_foo_exists']}")
                note(f"submit refine prompt: {REFINE_PROMPT!r}")
                send(REFINE_PROMPT); time.sleep(0.4); send("\r")
                refine_offset = cap_size(CAP); phase = "await_replan"; next_at = now + 1.0
        elif phase == "await_replan":
            names = wire_tool_uses(CAP, refine_offset)
            if any(n and "plan" in n.lower() for n in names):
                note(f"re-plan produced ExitPlanMode: {names} -> session usable after Esc")
                result["replan_tooluses"] = names
                result["replan_dialog"] = dialog_present(PTY_LOG)
                time.sleep(1.0); break
            elif quiet > 60:
                note("no re-plan in 60s"); result["replan_tooluses"] = names; break

    result["final_foo_exists"] = os.path.exists(foo)
    result["final_bar_exists"] = os.path.exists(bar)
    result["final_mode"] = footer_mode(PTY_LOG)

    send("\x1b"); time.sleep(0.2)
    send("/exit\r"); time.sleep(0.6); send("\x03"); send("\x04")
    logf.close()
    try:
        os.kill(pid, 15)
    except OSError:
        pass

    print("\n\n==== PROBE 3 RESULT (plan-approval Esc reject) ====")
    print(f"plan#1 tool_uses:           {result.get('plan1_tooluses')}")
    print(f"after Esc - footer mode:    {result.get('after_esc_mode')}  (expect: plan)")
    print(f"after Esc - dialog present: {result.get('after_esc_dialog_present')}  (expect: False)")
    print(f"after Esc - foo written:    {result.get('after_esc_foo_exists')}  (expect: False)")
    print(f"re-plan tool_uses:          {result.get('replan_tooluses')}  (expect ExitPlanMode -> usable)")
    print(f"final foo/bar exists:       foo={result.get('final_foo_exists')} bar={result.get('final_bar_exists')}")
    print(f"final mode:                 {result.get('final_mode')}")
    print("\n--- screen right after Esc ---")
    print(result.get("after_esc_screen", "(none)"))
    with open("/tmp/ao-planprobe3-result.json", "w") as f:
        json.dump(result, f, indent=2, default=str)
    print("\nfull -> /tmp/ao-planprobe3-result.json   pty -> " + PTY_LOG)


if __name__ == "__main__":
    main()
