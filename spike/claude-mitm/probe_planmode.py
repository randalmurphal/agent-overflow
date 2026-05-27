#!/usr/bin/env python3
"""Empirically resolve the BLOCKING question for spec §3.8 (plan mode):

  When AO launches Claude with BOTH `--permission-mode plan` and
  `--dangerously-skip-permissions`, does the turn start in PLAN mode
  (model presents a plan via ExitPlanMode, executes nothing) or in
  BYPASS (model just executes)?

Source (permissionSetup.ts:721-796) says orderedModes=[bypassPermissions, plan]
and the loop picks the first non-disabled mode + breaks => bypass wins. But the
local source lags the installed binary, so we confirm against the real CLI.

We also test `--permission-mode plan` ALONE (expected: plan) as the control,
and record the plan-approval option list the TUI renders (so §3.8 can state the
real option order, and whether "Yes, and bypass permissions" appears).

GROUND-TRUTH DISCRIMINATOR = filesystem:
  - foo.txt EXISTS after the turn  => a mutating tool executed => BYPASS
  - foo.txt ABSENT + ExitPlanMode tool_use on the wire => PLAN (model paused
    for approval, executed nothing)
Wire (Write/MultiEdit vs ExitPlanMode tool_use names) corroborates.
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
PROMPT = ("Create a file named foo.txt in the current directory containing "
          "exactly the text HELLO. Use the Write tool. Then stop.")
HARD_TIMEOUT_S = 90


def cap_lines(cap_path, start_offset):
    """Return (lines_after_offset, new_offset)."""
    try:
        with open(cap_path, "r", errors="replace") as f:
            f.seek(start_offset)
            data = f.read()
            return data.splitlines(), f.tell()
    except OSError:
        return [], start_offset


def tool_uses_in(cap_path, start_offset):
    """Scan SSE response chunks (only entries at/after start_offset) for
    tool_use content blocks; return the set of tool names seen."""
    lines, _ = cap_lines(cap_path, start_offset)
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


def set_winsize(fd, rows=50, cols=200):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def clean(b):
    t = b.decode("utf-8", "replace")
    t = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", t)
    t = re.sub(r"\x1b\][^\x07]*\x07", "", t)
    return "\n".join(l.rstrip() for l in t.splitlines() if l.strip())


def run_variant(name, extra_args, cap_offset):
    """Launch one claude variant, drive past trust, let the turn run, then
    inspect FS + wire. Returns a result dict."""
    work = f"/tmp/ao-planprobe-{name}"
    if os.path.exists(work):
        shutil.rmtree(work)
    os.makedirs(work)
    pty_log = f"/tmp/ao-planprobe-{name}.log"

    env = dict(os.environ)
    env["ANTHROPIC_BASE_URL"] = BASE_URL
    env["TERM"] = "xterm-256color"
    os.chdir(work)

    argv = ["claude", PROMPT] + extra_args
    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude", argv, env)
        os._exit(127)
    set_winsize(master)

    logf = open(pty_log, "wb")
    start = time.time()
    last_out = start
    trust_done = submitted = decided = False
    foo = os.path.join(work, "foo.txt")
    result = {"variant": name, "argv": argv[1:]}

    def send(s):
        os.write(master, s.encode() if isinstance(s, str) else s)

    def note(m):
        print(f"[{name} {time.time()-start:5.1f}s] {m}", file=sys.stderr)

    note(f"launch: claude <prompt> {' '.join(extra_args)}")
    while True:
        if time.time() - start > HARD_TIMEOUT_S:
            note("HARD TIMEOUT"); break
        r, _, _ = select.select([master], [], [], 0.3)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            logf.write(data); logf.flush()
            last_out = time.time()
            low = data[-4000:].lower()
            if not trust_done and b"trust" in low and b"?" in low:
                note("trust -> Enter"); send("\r"); trust_done = True
        quiet = time.time() - last_out

        # If the argv prompt didn't auto-submit, nudge it.
        if not submitted and trust_done and time.time() - start > 4 and quiet > 6:
            note("nudge submit"); send("\r"); submitted = True; last_out = time.time()

        # Decide as soon as we have a decisive signal.
        if not decided:
            names = tool_uses_in(CAP, cap_offset)
            foo_exists = os.path.exists(foo)
            mutating = [n for n in names if n in
                        ("Write", "MultiEdit", "Edit", "NotebookEdit", "Bash")]
            exitplan = [n for n in names if n and "plan" in n.lower()]
            if foo_exists or mutating:
                result["verdict"] = "BYPASS"; decided = True
            elif exitplan:
                result["verdict"] = "PLAN"; decided = True
            # give it a moment after decision to capture approval dialog text
            if decided:
                note(f"decided: {result['verdict']} (tools={names}, foo={foo_exists})")
                time.sleep(2.0)
                break

        # If the turn went totally quiet without a decisive signal, stop.
        if submitted and quiet > 14 and time.time() - start > 25:
            note("quiet, no decisive tool signal -> stop"); break

    # Final inspection.
    names = tool_uses_in(CAP, cap_offset)
    foo_exists = os.path.exists(foo)
    foo_content = open(foo).read() if foo_exists else None
    if "verdict" not in result:
        if foo_exists:
            result["verdict"] = "BYPASS"
        elif any(n and "plan" in n.lower() for n in names):
            result["verdict"] = "PLAN"
        else:
            result["verdict"] = "INCONCLUSIVE"
    tail = clean(open(pty_log, "rb").read())[-2200:]
    result.update({
        "tool_uses": names,
        "foo_exists": foo_exists,
        "foo_content": foo_content,
        "pty_tail": tail,
    })

    # Clean shutdown.
    send("\x1b"); time.sleep(0.2)
    send("/exit\r"); time.sleep(0.6); send("\x03"); send("\x04")
    logf.close()
    try:
        os.kill(pid, 15)
    except OSError:
        pass
    time.sleep(0.5)
    _, new_offset = cap_lines(CAP, 0)  # full size for next offset
    return result, new_offset


def main():
    # offset so each variant only sees its own wire traffic
    _, offset = cap_lines(CAP, 0)
    results = []

    r1, offset = run_variant(
        "combo", ["--permission-mode", "plan", "--dangerously-skip-permissions"],
        offset)
    results.append(r1)

    r2, offset = run_variant(
        "planonly", ["--permission-mode", "plan"], offset)
    results.append(r2)

    print("\n\n==== PLAN-MODE PROBE RESULTS ====")
    for r in results:
        print(f"\n--- variant: {r['variant']}  args={r['argv']}")
        print(f"  VERDICT:      {r['verdict']}")
        print(f"  tool_uses:    {r['tool_uses']}")
        print(f"  foo.txt:      exists={r['foo_exists']} content={r['foo_content']!r}")
        print(f"  pty tail (approval dialog if PLAN):\n{indent(r['pty_tail'])}")
    with open("/tmp/ao-planprobe-result.json", "w") as f:
        json.dump(results, f, indent=2, default=str)
    print("\nfull result -> /tmp/ao-planprobe-result.json")

    print("\n==== INTERPRETATION ====")
    combo = next(r for r in results if r["variant"] == "combo")
    planonly = next(r for r in results if r["variant"] == "planonly")
    print(f"combo (plan + skip-perms): {combo['verdict']}")
    print(f"plan alone:                {planonly['verdict']}")
    if combo["verdict"] == "BYPASS":
        print("=> CONFIRMS source: --dangerously-skip-permissions WINS over "
              "--permission-mode plan. §3.8 launch path must NOT use the combo "
              "to enter plan; bypass takes over.")
    elif combo["verdict"] == "PLAN":
        print("=> CONTRADICTS source: the combo DOES enter plan mode on the "
              "installed binary. §3.8 launch path is valid as written.")
    else:
        print("=> combo inconclusive; inspect pty tail + cap log.")


def indent(s):
    return "\n".join("    | " + l for l in s.splitlines())


if __name__ == "__main__":
    main()
