#!/usr/bin/env python3
"""Which permission world is interactive mode in?

Run interactive claude in DEFAULT permission mode (no skip, no acceptEdits) with
a prompt that REQUIRES a permission-gated tool (Bash), while passing a bad
--permission-prompt-tool. When the tool actually needs permission:

  - WIRED:   we see "(passed via --permission-prompt-tool) not found. Available
             MCP tools: ..." -> the flag drives permission in interactive, so AO
             could host a real MCP permission tool and surface prompts in its UI.
  - IGNORED: we see Claude's own TUI permission prompt ("Do you want to proceed",
             "1. Yes" / "2. No", "wants to run") -> --permission-prompt-tool is a
             no-op interactively; AO's only dynamic-permission surface is the TUI
             (scrape) — static policy otherwise.
"""
import os
import pty
import select
import struct
import fcntl
import termios
import re
import time

SPIKE_DIR = "/home/rmurphy/repos/agent-overflow-claude-mitm-spike"
BAD_TOOL = "mcp__nonexistent_server__decide"
# Write is NOT in the user allow-list (Read/Glob/Grep/git/safe-bash only), so in
# DEFAULT permission mode this forces a real permission request.
PROMPT = ("Use the Write tool to create the file /tmp/perm-force-test.txt with the "
          "exact contents: hello-from-claude . Do nothing else.")

WIRED_MARKERS = ("Available MCP tools", "not found")
TUI_PROMPT_MARKERS = ("Do you want to", "wants to", "1. Yes", "❯ 1.",
                      "Yes, allow", "No, and tell Claude", "proceed?", "Create file")


def set_winsize(fd, rows=50, cols=200):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def clean(b):
    t = b.decode("utf-8", "replace")
    t = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", t)
    t = re.sub(r"\x1b\][^\x07]*\x07", "", t)
    return "\n".join(l for l in t.splitlines() if l.strip())


def main():
    env = dict(os.environ)
    env["TERM"] = "xterm-256color"
    env["ANTHROPIC_BASE_URL"] = "http://127.0.0.1:8090"  # capture the turn too
    os.chdir(SPIKE_DIR)
    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude", ["claude", PROMPT, "--permission-prompt-tool", BAD_TOOL], env)
        os._exit(127)
    set_winsize(master)

    out = b""
    start = time.time()
    verdict = None
    trust_done = False
    submitted = False
    last_out = start
    while time.time() - start < 70:
        r, _, _ = select.select([master], [], [], 0.4)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            out += data
            last_out = time.time()
            tail = out[-3000:]
            low = tail.lower()
            if not trust_done and b"trust" in low and b"?" in low:
                os.write(master, b"\r"); trust_done = True
        full = out.decode("utf-8", "replace")
        if any(m in full for m in WIRED_MARKERS) and "permission-prompt-tool" in full:
            verdict = "WIRED"; break
        if any(m in full for m in WIRED_MARKERS) and "Available MCP tools" in full:
            verdict = "WIRED"; break
        if any(m in full for m in TUI_PROMPT_MARKERS):
            verdict = "IGNORED (TUI permission prompt shown)"; break
        # nudge submit if idle early (positional prompt sometimes needs Enter)
        if not submitted and time.time() - start > 8 and time.time() - last_out > 6:
            os.write(master, b"\r"); submitted = True; last_out = time.time()

    # decline whatever prompt is up, then exit
    try:
        if verdict and verdict.startswith("IGNORED"):
            os.write(master, b"2\r")  # choose No
            time.sleep(0.4)
        os.write(master, b"\x03"); time.sleep(0.2); os.write(master, b"\x03")
        os.write(master, b"/exit\r"); time.sleep(0.3)
        os.kill(pid, 15)
    except OSError:
        pass

    print(f"VERDICT: {verdict or 'INCONCLUSIVE (timed out)'}")
    print("---- cleaned tail (last ~2200 chars) ----")
    print(clean(out)[-2200:])
    print("---- end ----")


if __name__ == "__main__":
    main()
