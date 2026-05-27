#!/usr/bin/env python3
"""Drive a *multi-turn* interactive Claude session through a PTY.

Proves AO can interact with interactive mode as intended: submit a queue of
prompts one at a time into a live TUI, detecting each user-visible turn's
completion from the *proxy capture* (not PTY text, which echoes our own words).

Turn-complete signal (hardened, matches ao_transform.classify_request):
  a "completed agent turn" = a /v1/messages exchange whose REQUEST is an agent
  request (has tools AND max_tokens>1, i.e. not the max_tokens:1 quota preflight
  and not a tools-less auxiliary/title call) and whose RESPONSE ends with
  stop_reason == "end_turn" (a "tool_use" stop is mid-turn; more follows).

After each turn completes and the stream goes quiet, the next queued prompt is
typed into the TUI. After the last turn, we /exit cleanly.
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

SPIKE_DIR = "/home/rmurphy/repos/agent-overflow-claude-mitm-spike"
CAP = os.environ.get("AO_CAP_LOG", "/tmp/cap-multi.jsonl")
PTY_LOG = "/tmp/ao-multi-pty.log"
BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8090")

# Turn 1 triggers parallel Read tool_use (Read is allow-listed -> auto-runs, no
# permission prompt). Turn 2 is a pure-text follow-up (no tools) -> exercises the
# classifier (must NOT be dropped as auxiliary) and multi-turn continuity.
PROMPTS = [
    "Read these two files and report each one's line count: "
    "spike/claude-mitm/proxy/main.go and spike/claude-mitm/analyze.py",
    "Based only on what you just read, which of those two files has more lines? "
    "Answer in one short sentence.",
]

HARD_TIMEOUT_S = 240
QUIET_AFTER_TURN_S = 2.5
SUBMIT_FALLBACK_QUIET_S = 12.0


def completed_agent_turns(cap_path):
    """Count user-visible agent turns completed so far, from the proxy capture."""
    try:
        lines = open(cap_path, "r", errors="replace").read().splitlines()
    except OSError:
        return 0
    reqs = {}
    for line in lines:
        try:
            e = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        rid = e.get("req_id")
        if rid is None:
            continue
        r = reqs.setdefault(rid, {"chunks": []})
        if e.get("kind") == "request":
            r["path"] = e.get("path")
            r["body"] = e.get("body", "")
        elif e.get("kind") == "response_chunk":
            r["chunks"].append(e["text"])
    count = 0
    for r in reqs.values():
        if r.get("path") != "/v1/messages":
            continue
        try:
            b = json.loads(r.get("body", ""))
        except (json.JSONDecodeError, ValueError):
            continue
        if (b.get("max_tokens") or 0) <= 1:           # quota preflight
            continue
        if len(b.get("tools", []) or []) == 0:         # tools-less auxiliary/title
            continue
        raw = "".join(r["chunks"])
        # last stop_reason in this response
        stop = None
        for ln in raw.splitlines():
            if ln.startswith("data:"):
                try:
                    d = json.loads(ln[5:].strip())
                except (json.JSONDecodeError, ValueError):
                    continue
                if d.get("type") == "message_delta":
                    stop = d.get("delta", {}).get("stop_reason", stop)
        if stop == "end_turn":
            count += 1
    return count


def set_winsize(fd, rows=50, cols=200):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def main():
    # NOTE: the proxy owns CAP (open handle); start it on a fresh log per run
    # rather than unlinking the file out from under it here.
    env = dict(os.environ)
    env["ANTHROPIC_BASE_URL"] = BASE_URL
    env["TERM"] = "xterm-256color"
    os.chdir(SPIKE_DIR)

    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude", ["claude", PROMPTS[0]], env)
        os._exit(127)
    set_winsize(master)

    log = open(PTY_LOG, "wb")
    start = time.time()
    next_prompt_idx = 1                 # prompt 0 was passed positionally
    turns_target = 1                    # expect 1 completed turn per submitted prompt
    last_output_at = start
    trust_handled = False
    submit_fallback_sent = False
    last_seen_turns = 0
    turn_done_at = None

    def send(s):
        os.write(master, s.encode())

    while True:
        elapsed = time.time() - start
        if elapsed > HARD_TIMEOUT_S:
            print(f"[driver] hard timeout after {elapsed:.0f}s", file=sys.stderr)
            break

        r, _, _ = select.select([master], [], [], 0.5)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            log.write(data); log.flush()
            last_output_at = time.time()
            tail = data[-2000:].lower()
            if not trust_handled and b"trust" in tail and b"?" in tail:
                print(f"[driver] trust dialog -> Enter at {elapsed:.1f}s", file=sys.stderr)
                send("\r"); trust_handled = True

        turns = completed_agent_turns(CAP)
        if turns > last_seen_turns:
            last_seen_turns = turns
            turn_done_at = time.time()
            print(f"[driver] completed agent turn #{turns} at {elapsed:.1f}s", file=sys.stderr)

        # Submit fallback: positional prompt 0 sometimes needs an explicit Enter.
        quiet = time.time() - last_output_at
        if (not submit_fallback_sent and last_seen_turns == 0
                and elapsed > 6 and quiet > SUBMIT_FALLBACK_QUIET_S):
            print("[driver] nudging submit (Enter)", file=sys.stderr)
            send("\r"); submit_fallback_sent = True; last_output_at = time.time()

        # Once the current turn is done and the stream is quiet, advance.
        if (turn_done_at and last_seen_turns >= turns_target
                and (time.time() - turn_done_at) > QUIET_AFTER_TURN_S
                and quiet > 1.0):
            if next_prompt_idx < len(PROMPTS):
                p = PROMPTS[next_prompt_idx]
                print(f"[driver] submitting prompt #{next_prompt_idx}: {p[:50]!r}...", file=sys.stderr)
                send(p)
                time.sleep(0.4)
                send("\r")
                next_prompt_idx += 1
                turns_target += 1
                turn_done_at = None
                last_output_at = time.time()
            else:
                print("[driver] all prompts done -> /exit", file=sys.stderr)
                send("/exit\r")
                time.sleep(1.2)
                send("\x03"); send("\x04")
                break

    # drain
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
        os.kill(pid, 15)
    except OSError:
        pass
    print(f"[driver] done. turns={last_seen_turns} cap={CAP} pty={PTY_LOG}", file=sys.stderr)


if __name__ == "__main__":
    main()
