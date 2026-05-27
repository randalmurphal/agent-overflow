#!/usr/bin/env python3
"""Drive an interactive Claude Code TUI session through a PTY and exit cleanly.

This proves that *interactive* mode (no -p) routes /v1/messages through
ANTHROPIC_BASE_URL exactly like headless does, and produces a transcript we
can diff against. The prompt is passed positionally so Claude auto-submits it
on launch; we watch the PTY stream for a completion marker, then send /exit.
"""
import json
import os
import pty
import select
import struct
import sys
import termios
import fcntl
import time

PROMPT = "Run the bash command: echo spike-interactive-99999 — then reply with only the word DONE."
PTY_LOG = "/tmp/ao-interactive-pty.log"
CAP_LOG = os.environ.get("AO_CAP_LOG", "/tmp/cap-interactive.jsonl")
HARD_TIMEOUT_S = 120
QUIET_AFTER_DONE_S = 3.0
# If we land on the first-run trust dialog, its default is "Yes, trust" and
# it confirms on Enter. We also fall back to submitting a possibly-prefilled
# prompt if nothing happens for a while after trust is cleared.
SUBMIT_FALLBACK_QUIET_S = 12.0


def turn_complete(cap_path):
    """True once the proxy log shows an end_turn that follows a tool_use turn.

    Driving completion off the captured SSE (not the PTY text) avoids false
    positives from the prompt echo, which contains our own marker words. Each
    JSONL line is decoded so we match the *unescaped* SSE text in the chunks.
    """
    try:
        lines = open(cap_path, "r", errors="replace").read().splitlines()
    except OSError:
        return False
    saw_tool_use = False
    for line in lines:
        try:
            event = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        if event.get("kind") != "response_chunk":
            continue
        text = event.get("text", "")
        if '"stop_reason":"tool_use"' in text:
            saw_tool_use = True
        if saw_tool_use and '"stop_reason":"end_turn"' in text:
            return True
    return False


def set_winsize(fd, rows=50, cols=200):
    winsize = struct.pack("HHHH", rows, cols, 0, 0)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, winsize)


def main():
    env = dict(os.environ)
    env["ANTHROPIC_BASE_URL"] = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8090")
    # Keep the TUI from doing anything fancy with our fake terminal.
    env["TERM"] = "xterm-256color"

    # Run in an already-trusted directory so we don't hit the trust dialog
    # (overridable; the handler below is a safety net if we do).
    target_cwd = os.environ.get("AO_CWD")
    if target_cwd:
        os.chdir(target_cwd)

    pid, master = pty.fork()
    if pid == 0:  # child
        os.execvpe(
            "claude",
            ["claude", PROMPT, "--dangerously-skip-permissions"],
            env,
        )
        os._exit(127)

    set_winsize(master)

    log = open(PTY_LOG, "wb")
    start = time.time()
    completed_at = None
    sent_exit = False
    trust_handled = False
    submit_fallback_sent = False
    last_output_at = start
    buf_tail = ""

    while True:
        elapsed = time.time() - start
        if elapsed > HARD_TIMEOUT_S:
            print(f"[driver] hard timeout after {elapsed:.0f}s", file=sys.stderr)
            _send(master, "/exit\r")
            break

        r, _, _ = select.select([master], [], [], 0.5)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            log.write(data)
            log.flush()
            last_output_at = time.time()
            text = data.decode("utf-8", "replace")
            buf_tail = (buf_tail + text)[-4000:]
            low = buf_tail.lower()
            if not trust_handled and ("trust this folder" in low or "trust this dir" in low or "do you trust" in low):
                print(f"[driver] trust dialog detected at {elapsed:.1f}s -> Enter", file=sys.stderr)
                _send(master, "\r")
                trust_handled = True
                buf_tail = ""

        # Completion is detected from the captured SSE, not the PTY text.
        if completed_at is None and turn_complete(CAP_LOG):
            completed_at = time.time()
            print(f"[driver] turn complete (end_turn after tool_use) at {elapsed:.1f}s", file=sys.stderr)

        # Safety net: if the positional prompt was pre-filled but not submitted,
        # nudge it once after a quiet spell.
        quiet = time.time() - last_output_at
        if (not submit_fallback_sent and completed_at is None
                and elapsed > 6 and quiet > SUBMIT_FALLBACK_QUIET_S):
            print(f"[driver] no activity for {quiet:.0f}s, sending Enter to submit", file=sys.stderr)
            _send(master, "\r")
            submit_fallback_sent = True
            last_output_at = time.time()

        # Once the turn is complete and the stream has been quiet a moment, exit.
        if completed_at and not sent_exit and (time.time() - completed_at) > QUIET_AFTER_DONE_S:
            print("[driver] sending /exit", file=sys.stderr)
            _send(master, "/exit\r")
            sent_exit = True
            # give it a moment to tear down, then also try ctrl-c/ctrl-d
            time.sleep(1.5)
            _send(master, "\x03")  # ctrl-c
            _send(master, "\x04")  # ctrl-d
            break

    # Drain remaining output briefly.
    drain_until = time.time() + 2
    while time.time() < drain_until:
        r, _, _ = select.select([master], [], [], 0.3)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            log.write(data)
        else:
            break
    log.close()

    try:
        os.kill(pid, 0)
        os.waitpid(pid, os.WNOHANG)
    except OSError:
        pass
    print(f"[driver] done; pty log at {PTY_LOG}", file=sys.stderr)


def _send(master, s):
    os.write(master, s.encode())


if __name__ == "__main__":
    main()
