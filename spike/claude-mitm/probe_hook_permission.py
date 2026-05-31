#!/usr/bin/env python3
"""Probe: can a PreToolUse hook drive per-call permissions in INTERACTIVE mode?

The headline question for the AO interactive migration. In stream-json mode AO
gets per-call approvals via the `can_use_tool` control channel (gone in
interactive). This tests the replacement: a PreToolUse hook that intercepts a
tool BEFORE execution, blocks (as AO would while a human approves), and returns
allow/deny — with `allow` suppressing the TUI permission prompt entirely.

Isolation: a throwaway CLAUDE_CONFIG_DIR (copied creds only — pristine settings
so DEFAULT permission mode truly prompts) + a throwaway cwd. Nothing in the real
~/.claude is read for settings or written.

Outcome detection is filesystem + wire, never TUI scraping:
  - hook fired / payload shape  -> /tmp/aohook/payloads.jsonl
  - allow ran the tool          -> /tmp/aohook/out.txt exists
  - deny blocked the tool       -> /tmp/aohook/out.txt absent
  - claude blocked on the hook  -> ts gap between hook entry and tool_result

Usage: probe_hook_permission.py <allow|deny> [sleep_seconds]
Env:   AO_BASE_URL (proxy), e.g. http://127.0.0.1:8091
"""
import json
import os
import pty
import select
import struct
import fcntl
import termios
import shutil
import sys
import time

SPIKE_DIR = "/home/rmurphy/repos/agent-overflow-claude-mitm-spike/spike/claude-mitm"
HOOK = f"{SPIKE_DIR}/hook_relay.py"
CONFIG_DIR = "/tmp/aoclaude"
CWD = "/tmp/aocwd"
AOHOOK = "/tmp/aohook"
OUT = f"{AOHOOK}/out.txt"
PAYLOADS = f"{AOHOOK}/payloads.jsonl"
CTL = f"{AOHOOK}/ctl.json"
REAL_CREDS = os.path.expanduser("~/.claude/.credentials.json")
BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")

DECISION = sys.argv[1] if len(sys.argv) > 1 else "allow"
SLEEP_S = float(sys.argv[2]) if len(sys.argv) > 2 else 4.0
PROMPT = ("Use the Bash tool to run exactly this command and nothing else: "
          f"echo PROBE-OK > {OUT}")
PTY_LOG = f"{AOHOOK}/pty-{DECISION}.log"
HARD_TIMEOUT_S = 90


def set_winsize(fd, rows=50, cols=200):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


REAL_GLOBAL = os.path.expanduser("~/.claude.json")


def setup():
    os.makedirs(AOHOOK, exist_ok=True)
    os.makedirs(CWD, exist_ok=True)
    os.makedirs(CONFIG_DIR, exist_ok=True)
    # pristine creds copy so OAuth works without reading the rest of ~/.claude
    shutil.copy(REAL_CREDS, f"{CONFIG_DIR}/.credentials.json")
    # Seed GlobalConfig so onboarding (theme/login pickers) is skipped, and
    # pre-trust the throwaway cwd so the trust dialog never blocks the turn.
    # Only my hook lives in settings.json — the user's real hooks are NOT copied.
    try:
        gc = json.load(open(REAL_GLOBAL))
    except (OSError, json.JSONDecodeError):
        gc = {"numStartups": 5, "theme": "dark", "hasCompletedOnboarding": True}
    gc.setdefault("projects", {})
    gc["projects"][CWD] = {
        "hasTrustDialogAccepted": True,
        "hasCompletedProjectOnboarding": True,
        "allowedTools": [],
        "history": [],
    }
    with open(f"{CONFIG_DIR}/.claude.json", "w") as f:
        json.dump(gc, f)
    settings = {
        "hooks": {
            "PreToolUse": [
                {"hooks": [{"type": "command", "command": f"python3 {HOOK}"}]}
            ],
            "PostToolUse": [
                {"hooks": [{"type": "command", "command": f"python3 {HOOK}"}]}
            ],
        }
    }
    with open(f"{CONFIG_DIR}/settings.json", "w") as f:
        json.dump(settings, f)
    with open(CTL, "w") as f:
        json.dump({"decision": DECISION, "sleep": SLEEP_S, "schema": "modern"}, f)
    for p in (OUT, PAYLOADS):
        try:
            os.remove(p)
        except OSError:
            pass


def payload_summary():
    try:
        lines = open(PAYLOADS, errors="replace").read().splitlines()
    except OSError:
        return []
    rows = []
    for ln in lines:
        try:
            e = json.loads(ln)
        except (json.JSONDecodeError, ValueError):
            continue
        rows.append((e.get("event"), e.get("tool"), e.get("ts")))
    return rows


def main():
    setup()
    env = dict(os.environ)
    env["CLAUDE_CONFIG_DIR"] = CONFIG_DIR
    env["ANTHROPIC_BASE_URL"] = BASE_URL
    env["TERM"] = "xterm-256color"
    os.chdir(CWD)

    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude", ["claude", PROMPT], env)
        os._exit(127)
    set_winsize(master)

    log = open(PTY_LOG, "wb")
    start = time.time()
    last_out = start
    trust_handled = False
    submit_nudged = False
    saw_tui_perm_prompt = False
    hook_fire_ts = None

    def send(s):
        os.write(master, s.encode())

    while time.time() - start < HARD_TIMEOUT_S:
        elapsed = time.time() - start
        r, _, _ = select.select([master], [], [], 0.4)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            log.write(data); log.flush()
            last_out = time.time()
            tail = data.lower()
            # trust / onboarding: Enter answers the default
            if not trust_handled and b"trust" in tail and b"?" in tail:
                send("\r"); trust_handled = True
            # detect (corroborating only) whether a TUI permission prompt showed
            for m in (b"do you want to", b"wants to run", b"1. yes", b"\xe2\x9d\xaf 1."):
                if m in tail:
                    saw_tui_perm_prompt = True

        rows = payload_summary()
        pre = [r for r in rows if r[0] == "PreToolUse"]
        if pre and hook_fire_ts is None:
            hook_fire_ts = time.time()

        # submit nudge if positional prompt didn't auto-send
        quiet = time.time() - last_out
        if not submit_nudged and not pre and elapsed > 8 and quiet > 8:
            send("\r"); submit_nudged = True; last_out = time.time()

        # done conditions
        if os.path.exists(OUT):
            time.sleep(0.6)
            break
        if hook_fire_ts and time.time() - hook_fire_ts > 14:
            break  # deny case (no out file) or stalled — stop waiting

    # exit cleanly
    try:
        send("/exit\r"); time.sleep(0.6)
        send("\x03"); send("\x03"); send("\x04")
        time.sleep(0.3)
        os.kill(pid, 15)
    except OSError:
        pass
    log.close()

    rows = payload_summary()
    out_exists = os.path.exists(OUT)
    out_body = ""
    if out_exists:
        out_body = open(OUT, errors="replace").read().strip()
    print("==== HOOK PERMISSION PROBE RESULT ====")
    print(f"decision   : {DECISION}  (hook sleep {SLEEP_S}s)")
    print(f"hook fired : {len(rows)} hook events; PreToolUse tools="
          f"{[r[1] for r in rows if r[0]=='PreToolUse']}")
    print(f"            PostToolUse tools={[r[1] for r in rows if r[0]=='PostToolUse']}")
    print(f"out.txt    : exists={out_exists} body={out_body!r}")
    print(f"tui prompt : saw_tui_permission_prompt={saw_tui_perm_prompt}")
    if hook_fire_ts:
        print(f"timing     : first hook fired at {hook_fire_ts-start:.1f}s")
    print(f"pty log    : {PTY_LOG}")
    print("======================================")


if __name__ == "__main__":
    main()
