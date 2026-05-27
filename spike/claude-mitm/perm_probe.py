#!/usr/bin/env python3
"""Does INTERACTIVE claude honor --permission-prompt-tool, or is it print-only?

The clean per-call permission surface (`can_use_tool` control_request) rides the
stream-json control channel, which is --print-only. The only *dynamic* permission
path that might reach interactive mode is --permission-prompt-tool <mcp_tool>.

Test: launch interactive `claude` with a deliberately-nonexistent MCP tool name.
  - If the flag is PROCESSED in interactive mode, arg validation fails fast with
    "(passed via --permission-prompt-tool) not found. Available MCP tools: ..."
    (string present in the binary) -> dynamic permission *is* reachable interactively.
  - If the flag is IGNORED (print-only), the normal TUI banner appears with no such
    error -> interactive has no dynamic-permission hook; AO is limited to static
    policy (--permission-mode / allow-deny / settings) or TUI scraping.
"""
import os
import pty
import select
import struct
import fcntl
import termios
import sys
import time

SPIKE_DIR = "/home/rmurphy/repos/agent-overflow-claude-mitm-spike"
BAD_TOOL = "mcp__nonexistent_server__decide"


def set_winsize(fd, rows=50, cols=200):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def run(args, label, seconds=8):
    env = dict(os.environ)
    env["TERM"] = "xterm-256color"
    os.chdir(SPIKE_DIR)
    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude", ["claude", *args], env)
        os._exit(127)
    set_winsize(master)
    out = b""
    start = time.time()
    while time.time() - start < seconds:
        r, _, _ = select.select([master], [], [], 0.4)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            out += data
            # If a trust dialog shows, accept it so we reach steady state.
            low = out[-2000:].lower()
            if b"trust" in low and b"?" in low:
                os.write(master, b"\r")
        # Stop early once we've clearly errored or clearly reached the TUI.
        if b"Available MCP tools" in out or b"not found" in out:
            break
    # tear down
    try:
        os.write(master, b"\x03"); time.sleep(0.2); os.write(master, b"\x03")
        os.kill(pid, 15)
    except OSError:
        pass
    text = out.decode("utf-8", "replace")
    processed = ("permission-prompt-tool" in text and "not found" in text) or \
                "Available MCP tools" in text
    print(f"\n===== {label} =====")
    print(f"args: {args}")
    print(f"--permission-prompt-tool PROCESSED in interactive? {processed}")
    # Show the most telling ~1200 chars (strip ANSI noise lightly).
    import re
    clean = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", text)
    clean = re.sub(r"\x1b\][^\x07]*\x07", "", clean)
    clean = "\n".join(l for l in clean.splitlines() if l.strip())
    snippet = clean[:1600]
    print("---- output (cleaned) ----")
    print(snippet)
    print("---- end ----")
    return processed


if __name__ == "__main__":
    # 1) interactive (no -p) with a bad permission-prompt-tool
    run(["--permission-prompt-tool", BAD_TOOL], "INTERACTIVE + bad --permission-prompt-tool")
    # 2) control: headless (-p) with the same bad tool — known to validate the flag
    run(["-p", "hi", "--permission-prompt-tool", BAD_TOOL], "HEADLESS(-p) + bad --permission-prompt-tool (control)")
