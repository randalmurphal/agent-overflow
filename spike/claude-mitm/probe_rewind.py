#!/usr/bin/env python3
"""Empirically validate Claude Code's REWIND (revert) flow end-to-end through a PTY.

Goal: confirm against the *real 2.1.150 binary* (not just source) that
  (1) `/rewind` opens the MessageSelector,
  (2) selecting a message that had file changes after it offers the scope
      sub-menu ("Restore code and conversation" / "Restore conversation" /
      "Restore code"),
  (3) choosing "Restore code and conversation" actually reverts the file ON DISK,
  (4) the rewind is observable on the wire (next agent request's `messages`
      array shrinks),
and to CAPTURE THE REAL ANSI of both selector levels so AO's detector has real
markers to parse (the spec's detection rules must reference the actual stream).

Scenario (file side-effect is the oracle):
  turn0: create foo.txt = ORIGINAL
  turn1: change foo.txt = MODIFIED
  /rewind -> select the turn1 user message -> "Restore code and conversation"
  assert foo.txt == ORIGINAL again.

Launched with --permission-mode acceptEdits so Write/Edit auto-run (no perm
prompt), pointed at the logging proxy on AO_BASE_URL.
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

WORK = "/tmp/ao-rewind-probe"
FOO = os.path.join(WORK, "foo.txt")
CAP = os.environ.get("AO_CAP_LOG", "/tmp/cap-rewind.jsonl")
PTY_LOG = "/tmp/ao-rewind-pty.log"
BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8090")

PROMPTS = [
    "Create a file named foo.txt in the current directory whose entire contents "
    "are exactly the single line: ORIGINAL . Do nothing else.",
    "Now make foo.txt contain exactly the single line: MODIFIED . Do nothing else.",
]

HARD_TIMEOUT_S = 300
QUIET_AFTER_TURN_S = 2.5


def completed_agent_turns(cap_path):
    """User-visible agent turns completed (agent request -> end_turn)."""
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
        if (b.get("max_tokens") or 0) <= 1:
            continue
        if len(b.get("tools", []) or []) == 0:
            continue
        stop = None
        for ln in "".join(r["chunks"]).splitlines():
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


def agent_request_msg_counts(cap_path):
    """Ordered list of len(messages) for each agent request — to SEE the rewind
    truncate the history on the wire."""
    try:
        lines = open(cap_path, "r", errors="replace").read().splitlines()
    except OSError:
        return []
    out = []
    for line in lines:
        try:
            e = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        if e.get("kind") != "request" or e.get("path") != "/v1/messages":
            continue
        try:
            b = json.loads(e.get("body", ""))
        except (json.JSONDecodeError, ValueError):
            continue
        if (b.get("max_tokens") or 0) <= 1:
            continue
        if len(b.get("tools", []) or []) == 0:
            continue
        out.append(len(b.get("messages", [])))
    return out


def read_foo():
    try:
        return open(FOO).read().strip()
    except OSError:
        return "<absent>"


def list_sessions():
    """Transcript .jsonl files for this cwd's project dir (to see fork-as-new-file
    vs in-place). Claude encodes cwd as the project dir name (slashes -> dashes)."""
    import glob
    proj = os.path.expanduser("~/.claude/projects/" + WORK.replace("/", "-"))
    out = []
    for p in sorted(glob.glob(proj + "/*.jsonl")):
        try:
            out.append({"file": os.path.basename(p), "lines": sum(1 for _ in open(p, errors="replace"))})
        except OSError:
            pass
    return out


def set_winsize(fd, rows=50, cols=200):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def main():
    if os.path.exists(WORK):
        shutil.rmtree(WORK)
    os.makedirs(WORK)

    env = dict(os.environ)
    env["ANTHROPIC_BASE_URL"] = BASE_URL
    env["TERM"] = "xterm-256color"
    os.chdir(WORK)

    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude",
                   ["claude", PROMPTS[0], "--permission-mode", "acceptEdits"], env)
        os._exit(127)
    set_winsize(master)

    logf = open(PTY_LOG, "wb")
    start = time.time()
    last_out = start
    trust_done = False
    submitted0 = False

    phase = "turn0"            # turn0 -> turn1 -> open_rewind -> pick_msg -> pick_scope -> verify -> done
    turns_target = 1
    last_turns = 0
    turn_done_at = None
    phase_at = start
    foo_after_turn1 = None
    rewind_marks = {}

    def send(s):
        os.write(master, s.encode() if isinstance(s, str) else s)

    def note(msg):
        print(f"[probe {time.time()-start:5.1f}s] {msg}", file=sys.stderr)

    while True:
        if time.time() - start > HARD_TIMEOUT_S:
            note("HARD TIMEOUT"); break

        r, _, _ = select.select([master], [], [], 0.4)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            logf.write(data); logf.flush()
            last_out = time.time()
            low = data[-3000:].lower()
            if not trust_done and b"trust" in low and b"?" in low:
                note("trust dialog -> Enter"); send("\r"); trust_done = True

        quiet = time.time() - last_out
        turns = completed_agent_turns(CAP)
        if turns > last_turns:
            last_turns = turns
            turn_done_at = time.time()
            note(f"completed agent turn #{turns}")

        # submit-0 nudge (positional prompt sometimes needs Enter)
        if (not submitted0 and last_turns == 0 and trust_done
                and time.time() - start > 5 and quiet > 8):
            note("nudge submit prompt0"); send("\r")
            submitted0 = True; last_out = time.time()

        if phase == "turn0" and last_turns >= 1 and turn_done_at \
                and time.time() - turn_done_at > QUIET_AFTER_TURN_S and quiet > 1.0:
            note(f"after turn0 foo.txt={read_foo()!r}")
            note(f"submit prompt1: {PROMPTS[1][:40]!r}")
            send(PROMPTS[1]); time.sleep(0.4); send("\r")
            turns_target = 2; phase = "turn1"; turn_done_at = None
            last_out = time.time()

        elif phase == "turn1" and last_turns >= 2 and turn_done_at \
                and time.time() - turn_done_at > QUIET_AFTER_TURN_S and quiet > 1.0:
            foo_after_turn1 = read_foo()
            note(f"after turn1 foo.txt={foo_after_turn1!r}  (expect MODIFIED)")
            note("open /rewind")
            send("/rewind"); time.sleep(0.4); send("\r")
            phase = "open_rewind"; phase_at = time.time(); last_out = time.time()

        elif phase == "open_rewind" and time.time() - phase_at > 2.0:
            # MessageSelector should be up. Capture a snapshot of what we have.
            full = open(PTY_LOG, "rb").read().decode("utf-8", "replace")
            rewind_marks["level1_tail"] = full[-2500:]
            note("nav up to the turn1 user message, then Enter to select it")
            send("\x1b[A")          # up: sentinel(current) -> last real msg (turn1)
            time.sleep(0.6)
            send("\r")              # select that message -> scope submenu
            phase = "pick_scope"; phase_at = time.time(); last_out = time.time()

        elif phase == "pick_scope":
            full = open(PTY_LOG, "rb").read().decode("utf-8", "replace")
            if "Restore code and conversation" in full:
                rewind_marks["level2_seen"] = True
                rewind_marks["level2_tail"] = full[-2500:]
                note("scope submenu visible -> select default 'Restore code and conversation'")
                send("\r")          # 'both' is the default top option
                phase = "verify"; phase_at = time.time(); last_out = time.time()
            elif time.time() - phase_at > 12:
                rewind_marks["level2_seen"] = False
                note("scope submenu NOT seen within 12s; tail follows")
                rewind_marks["level2_tail"] = full[-2500:]
                phase = "verify"; phase_at = time.time()

        elif phase == "verify" and time.time() - phase_at > 4.0 and quiet > 1.5:
            foo_final = read_foo()
            note(f"after rewind foo.txt={foo_final!r}  (expect ORIGINAL)")
            rewind_marks["foo_after_turn1"] = foo_after_turn1
            rewind_marks["foo_final"] = foo_final
            rewind_marks["msg_counts_before_postprompt"] = agent_request_msg_counts(CAP)
            rewind_marks["sessions_after_rewind"] = list_sessions()
            # Send a post-rewind prompt to OBSERVE the wire truncation: the next
            # agent request's messages array must be SHORTER than pre-rewind tail.
            note("submit post-rewind prompt to observe wire truncation")
            send("Reply with exactly one word: ok"); time.sleep(0.4); send("\r")
            turns_target = 3; phase = "postrewind"; turn_done_at = None
            phase_at = time.time(); last_out = time.time()

        elif phase == "postrewind" and last_turns >= 3 and turn_done_at \
                and time.time() - turn_done_at > QUIET_AFTER_TURN_S and quiet > 1.0:
            rewind_marks["msg_counts_after_postprompt"] = agent_request_msg_counts(CAP)
            rewind_marks["sessions_after_postprompt"] = list_sessions()
            note("exit")
            send("\x1b"); time.sleep(0.2)
            send("/exit\r"); time.sleep(0.8)
            send("\x03"); send("\x04")
            phase = "done"; break

    # drain
    drain = time.time() + 2
    while time.time() < drain:
        r, _, _ = select.select([master], [], [], 0.3)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            logf.write(data)
        else:
            break
    logf.close()
    try:
        os.kill(pid, 15)
    except OSError:
        pass

    print("\n==== REWIND PROBE RESULT ====")
    print(f"foo after turn1 (expect MODIFIED): {rewind_marks.get('foo_after_turn1')!r}")
    print(f"foo after rewind (expect ORIGINAL): {rewind_marks.get('foo_final')!r}")
    print(f"scope submenu offered code+conversation: {rewind_marks.get('level2_seen')}")
    print(f"agent-req msg counts BEFORE post-rewind prompt: "
          f"{rewind_marks.get('msg_counts_before_postprompt')}")
    print(f"agent-req msg counts AFTER  post-rewind prompt: "
          f"{rewind_marks.get('msg_counts_after_postprompt')}  "
          f"(last entry should be SHORTER than the pre-rewind tail = wire truncation)")
    print(f"transcript sessions after rewind:      {rewind_marks.get('sessions_after_rewind')}")
    print(f"transcript sessions after post-prompt: {rewind_marks.get('sessions_after_postprompt')}")
    fa, ff = rewind_marks.get("foo_after_turn1"), rewind_marks.get("foo_final")
    verdict = ("PASS: file reverted on disk via TUI rewind"
               if (fa and "MODIFIED" in fa and ff and "ORIGINAL" in ff and "MODIFIED" not in ff)
               else "CHECK MANUALLY (see captured tails / pty log)")
    print(f"VERDICT: {verdict}")
    print(f"pty log: {PTY_LOG}   capture: {CAP}")
    # persist markers for spec authoring
    with open("/tmp/ao-rewind-marks.json", "w") as f:
        json.dump(rewind_marks, f, indent=2, default=str)
    print("markers -> /tmp/ao-rewind-marks.json")


if __name__ == "__main__":
    main()
