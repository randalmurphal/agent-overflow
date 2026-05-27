#!/usr/bin/env python3
"""Probe 2 — verify the ONLY viable plan-mode design (option A) end to end:

  Launch bypass (`--dangerously-skip-permissions`, NO --permission-mode so the
  flag doesn't lose the orderedModes race), then at runtime shift+tab into plan,
  submit a planning task, let the model ExitPlanMode, and approve with idx0 to
  land back in bypass.

One PTY session answers all gating unknowns:
  1. is auto mode available on this account? (auto would steal idx0 from bypass)
  2. does shift+tab from bypass reach plan, and what is the cycle? (auto between?)
  3. the EXACT plan-approval option list/order the TUI renders with bypass
     available (so §3.8 can state idx0 verbatim from the binary, not source)
  4. end-to-end: after `\r` on idx0, does the footer flip to bypass, does the
     file get written, and does post-approval execution avoid permission prompts?

Mode is read ONLY from the footer mode-indicator line (a bounded, fixed-format
structured PTY signal). Plan-ready is detected on the WIRE (ExitPlanMode
tool_use). We never scrape message content.
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
WORK = "/tmp/ao-planprobe2"
PTY_LOG = "/tmp/ao-planprobe2.log"
PLAN_PROMPT = "Add a file foo.txt containing HELLO to this directory."
HARD_TIMEOUT_S = 170
SHIFT_TAB = "\x1b[Z"


def clean(b):
    t = b.decode("utf-8", "replace") if isinstance(b, (bytes, bytearray)) else b
    t = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", t)
    t = re.sub(r"\x1b\][^\x07]*\x07", "", t)
    t = re.sub(r"\x1b[=>]", "", t)
    return "\n".join(l.rstrip() for l in t.splitlines() if l.strip())


def footer_mode(pty_log):
    """Read the current permission-mode from the footer mode-indicator line.
    Returns one of: bypass, plan, acceptEdits, auto, default, or None.
    Scans the tail of the rendered log; takes the LAST mode token seen."""
    try:
        full = open(pty_log, "rb").read()
    except OSError:
        return None
    tail = clean(full)[-4000:]
    norm = re.sub(r"\s+", "", tail.lower())
    # find all occurrences in order, return the last
    hits = []
    for token, mode in [
        ("bypasspermissionson", "bypass"),
        ("planmodeon", "plan"),
        ("accepteditson", "acceptEdits"),
        ("automodeon", "auto"),
    ]:
        idx = 0
        while True:
            i = norm.find(token, idx)
            if i < 0:
                break
            hits.append((i, mode))
            idx = i + 1
    if not hits:
        return "default"  # default mode shows no special indicator
    hits.sort()
    return hits[-1][1]


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


def cap_size(cap_path):
    try:
        return os.path.getsize(cap_path)
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
    cap_offset = cap_size(CAP)

    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude", ["claude", "--dangerously-skip-permissions"], env)
        os._exit(127)
    set_winsize(master)

    logf = open(PTY_LOG, "wb")
    start = time.time()
    last_out = start
    result = {"cycle": [], "events": []}

    # state machine
    phase = "boot"   # boot -> idle -> cycling -> submitted -> planready -> approved -> done
    trust_done = False
    cycle_sends = 0
    next_action_at = 0
    approval_offset = None

    def send(s):
        os.write(master, s.encode() if isinstance(s, str) else s)

    def note(m):
        msg = f"[p2 {time.time()-start:5.1f}s] {m}"
        print(msg, file=sys.stderr)
        result["events"].append(msg)

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
            logf.write(data); logf.flush()
            last_out = now
            low = data[-4000:].lower()
            if not trust_done and b"trust" in low and b"?" in low:
                note("trust -> Enter"); send("\r"); trust_done = True
                next_action_at = now + 2.5
        quiet = now - last_out

        if phase == "boot":
            # once trust cleared (or no trust shown) and things settle, read idle footer
            if (trust_done or now - start > 6) and quiet > 2.0 and now >= next_action_at:
                m = footer_mode(PTY_LOG)
                result["idle_mode"] = m
                full_screen = clean(open(PTY_LOG, "rb").read())[-2500:]
                result["idle_screen_tail"] = full_screen
                result["auto_available_hint"] = ("auto" in full_screen.lower())
                note(f"idle footer mode = {m}; auto-hint={result['auto_available_hint']}")
                phase = "cycling"; next_action_at = now + 0.3

        elif phase == "cycling":
            if now >= next_action_at:
                m = footer_mode(PTY_LOG)
                if m == "plan":
                    note(f"reached plan mode after {cycle_sends} shift+tab")
                    result["shift_tabs_to_plan"] = cycle_sends
                    note(f"submit plan prompt: {PLAN_PROMPT!r}")
                    send(PLAN_PROMPT); time.sleep(0.4); send("\r")
                    approval_offset = cap_size(CAP)
                    phase = "submitted"; next_action_at = now + 1.0
                elif cycle_sends >= 7:
                    note(f"gave up cycling after {cycle_sends}; stuck at {m}")
                    result["shift_tabs_to_plan"] = None
                    break
                else:
                    result["cycle"].append({"after_sends": cycle_sends, "mode": m})
                    send(SHIFT_TAB); cycle_sends += 1
                    note(f"shift+tab #{cycle_sends} (was {m})")
                    next_action_at = now + 0.7

        elif phase == "submitted":
            names = wire_tool_uses(CAP, approval_offset)
            if any(n and "plan" in n.lower() for n in names):
                note(f"ExitPlanMode tool_use on wire: {names} -> plan ready")
                result["plan_turn_tool_uses"] = names
                time.sleep(1.8)  # let approval dialog render
                screen = clean(open(PTY_LOG, "rb").read())[-3000:]
                result["approval_screen"] = screen
                note("captured approval screen; sending Enter (idx0)")
                approve_offset = cap_size(CAP)
                result["_approve_offset"] = approve_offset
                send("\r")
                phase = "approved"; next_action_at = now + 3.0
            elif quiet > 50:
                note("no ExitPlanMode within 50s quiet -> stop")
                result["plan_turn_tool_uses"] = names
                break

        elif phase == "approved":
            if now >= next_action_at:
                m = footer_mode(PTY_LOG)
                result["post_approval_mode"] = m
                result["post_approval_foo_exists"] = os.path.exists(foo)
                names_after = wire_tool_uses(CAP, result.get("_approve_offset", 0))
                result["post_approval_tool_uses"] = names_after
                note(f"post-approval: mode={m} foo={os.path.exists(foo)} tools={names_after}")
                # let any execution finish, check for permission prompt
                if os.path.exists(foo) or quiet > 25:
                    screen = clean(open(PTY_LOG, "rb").read())[-2500:]
                    result["post_approval_screen"] = screen
                    result["post_approval_had_perm_prompt"] = any(
                        s in screen for s in ("Do you want", "❯ 1.", "approve", "Allow"))
                    note("done"); break

    # final
    result["final_foo_exists"] = os.path.exists(foo)
    result["final_foo_content"] = open(foo).read() if os.path.exists(foo) else None
    result["final_mode"] = footer_mode(PTY_LOG)

    send("\x1b"); time.sleep(0.2)
    send("/exit\r"); time.sleep(0.6); send("\x03"); send("\x04")
    logf.close()
    try:
        os.kill(pid, 15)
    except OSError:
        pass

    print("\n\n==== PROBE 2 RESULT (plan-mode option-A design) ====")
    print(f"idle mode at launch:        {result.get('idle_mode')}")
    print(f"auto-available hint:        {result.get('auto_available_hint')}")
    print(f"cycle observed:             {result.get('cycle')}")
    print(f"shift+tabs to reach plan:   {result.get('shift_tabs_to_plan')}")
    print(f"plan-turn tool_uses (wire): {result.get('plan_turn_tool_uses')}")
    print(f"post-approval mode:         {result.get('post_approval_mode')}")
    print(f"post-approval foo exists:   {result.get('post_approval_foo_exists')}")
    print(f"post-approval tool_uses:    {result.get('post_approval_tool_uses')}")
    print(f"post-approval perm prompt:  {result.get('post_approval_had_perm_prompt')}")
    print(f"final foo:                  exists={result.get('final_foo_exists')} content={result.get('final_foo_content')!r}")
    print(f"final mode:                 {result.get('final_mode')}")
    print("\n--- APPROVAL SCREEN (exact option labels/order) ---")
    print(result.get("approval_screen", "(not captured)"))
    print("\n--- IDLE SCREEN TAIL (auto availability) ---")
    print(result.get("idle_screen_tail", "(not captured)"))
    with open("/tmp/ao-planprobe2-result.json", "w") as f:
        json.dump(result, f, indent=2, default=str)
    print("\nfull -> /tmp/ao-planprobe2-result.json   pty -> " + PTY_LOG)


if __name__ == "__main__":
    main()
