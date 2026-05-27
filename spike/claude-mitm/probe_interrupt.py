#!/usr/bin/env python3
"""Empirically characterize INTERRUPT (Esc while the agent is working).

AO must drive interrupt and must know how an interrupted turn looks on BOTH
channels, because it affects how AO's event-stream transform terminates a turn:

  PTY:   what marks the interrupt (so AO can show "stopped")?
  WIRE:  does the in-flight /v1/messages end with a clean
         message_delta(stop_reason)+message_stop, or is the SSE aborted
         mid-stream (connection closed, no message_stop)?  -> the transform's
         turn-finalization rule depends on this.
  AFTER: is the session immediately usable (submit a new prompt post-interrupt)?
         and does the partial assistant text survive into the next request's
         `messages` (so AO knows what was actually emitted before the stop)?

Method: ask for a long generation, wait until the proxy shows SSE chunks
flowing (turn in flight), send Esc, then inspect proxy + PTY, then submit a
follow-up prompt to confirm usability.
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

WORK = "/tmp/ao-interrupt-probe"
CAP = os.environ.get("AO_CAP_LOG", "/tmp/cap-interrupt.jsonl")
PTY_LOG = "/tmp/ao-interrupt-pty.log"
BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8092")

PROMPT0 = ("Write a thorough 900-word explanation of how the TCP three-way "
           "handshake works, in plain prose. Do not use any tools.")
PROMPT1 = "Reply with exactly one word: recovered"

HARD_TIMEOUT_S = 180


def in_flight_streaming(cap_path):
    """True once an agent /v1/messages request has begun streaming response
    chunks but has not yet ended — i.e. a turn is mid-flight (safe to interrupt)."""
    try:
        lines = open(cap_path, "r", errors="replace").read().splitlines()
    except OSError:
        return False, {}
    reqs = {}
    for line in lines:
        try:
            e = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        rid = e.get("req_id")
        if rid is None:
            continue
        r = reqs.setdefault(rid, {"chunks": 0, "ended": False})
        k = e.get("kind")
        if k == "request":
            r["path"] = e.get("path"); r["body"] = e.get("body", "")
        elif k == "response_chunk":
            r["chunks"] += 1
        elif k == "response_end":
            r["ended"] = True
    for r in reqs.values():
        if r.get("path") != "/v1/messages":
            continue
        try:
            b = json.loads(r.get("body", ""))
        except (json.JSONDecodeError, ValueError):
            continue
        if (b.get("max_tokens") or 0) <= 1 or len(b.get("tools", []) or []) == 0:
            continue
        if r["chunks"] > 0 and not r["ended"]:
            return True, r
    return False, {}


def analyze_interrupted_request(cap_path):
    """Find the agent request that was in flight when we interrupted; report
    whether its SSE carried a final message_delta stop_reason / message_stop, or
    was aborted, and whether the proxy logged an upstream_read error."""
    try:
        lines = open(cap_path, "r", errors="replace").read().splitlines()
    except OSError:
        return {}
    reqs = {}
    for line in lines:
        try:
            e = json.loads(line)
        except (json.JSONDecodeError, ValueError):
            continue
        rid = e.get("req_id")
        if rid is None:
            continue
        r = reqs.setdefault(rid, {"chunks": [], "errors": [], "ended": False})
        k = e.get("kind")
        if k == "request":
            r["path"] = e.get("path"); r["body"] = e.get("body", "")
        elif k == "response_chunk":
            r["chunks"].append(e["text"])
        elif k == "response_end":
            r["ended"] = True
        elif k == "error":
            r["errors"].append({"stage": e.get("stage"), "error": e.get("error")})
    # last agent request
    agent = [r for r in reqs.values()
             if r.get("path") == "/v1/messages"
             and _is_agent(r.get("body", ""))]
    if not agent:
        return {}
    r = agent[-1]
    raw = "".join(r["chunks"])
    sse_types = []
    stop_reason = None
    for ln in raw.splitlines():
        if ln.startswith("data:"):
            try:
                d = json.loads(ln[5:].strip())
            except (json.JSONDecodeError, ValueError):
                continue
            t = d.get("type")
            if t:
                sse_types.append(t)
            if t == "message_delta":
                stop_reason = d.get("delta", {}).get("stop_reason", stop_reason)
    return {
        "had_message_stop": "message_stop" in sse_types,
        "stop_reason": stop_reason,
        "last_sse_events": sse_types[-6:],
        "proxy_errors": r["errors"],
        "response_end_logged": r["ended"],
        "n_sse_events": len(sse_types),
    }


def _is_agent(body):
    try:
        b = json.loads(body)
    except (json.JSONDecodeError, ValueError):
        return False
    return (b.get("max_tokens") or 0) > 1 and len(b.get("tools", []) or []) > 0


def agent_msg_counts(cap_path):
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
        if _is_agent(e.get("body", "")):
            try:
                out.append(len(json.loads(e["body"]).get("messages", [])))
            except (json.JSONDecodeError, ValueError):
                pass
    return out


def set_winsize(fd, rows=50, cols=200):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def clean(b):
    import re
    t = b.decode("utf-8", "replace")
    t = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", t)
    t = re.sub(r"\x1b\][^\x07]*\x07", "", t)
    return "\n".join(l.rstrip() for l in t.splitlines() if l.strip())


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
        os.execvpe("claude", ["claude", PROMPT0, "--permission-mode", "acceptEdits"], env)
        os._exit(127)
    set_winsize(master)

    logf = open(PTY_LOG, "wb")
    start = time.time()
    last_out = start
    trust_done = submitted0 = interrupted = followed_up = False
    interrupt_at = None
    result = {}

    def send(s):
        os.write(master, s.encode() if isinstance(s, str) else s)

    def note(m):
        print(f"[intr {time.time()-start:5.1f}s] {m}", file=sys.stderr)

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
            low = data[-3000:].lower()
            if not trust_done and b"trust" in low and b"?" in low:
                note("trust -> Enter"); send("\r"); trust_done = True
        quiet = time.time() - last_out

        if (not submitted0 and trust_done and time.time() - start > 5 and quiet > 8):
            note("nudge submit"); send("\r"); submitted0 = True; last_out = time.time()

        # interrupt once the turn is visibly streaming
        if not interrupted:
            flowing, _ = in_flight_streaming(CAP)
            if flowing:
                time.sleep(0.8)              # let a few hundred tokens stream
                note("turn in flight -> send Esc (interrupt)")
                send("\x1b")
                interrupted = True; interrupt_at = time.time(); last_out = time.time()

        # after interrupt settles, inspect + submit follow-up
        if interrupted and not followed_up and interrupt_at \
                and time.time() - interrupt_at > 4.0:
            result["interrupt_wire"] = analyze_interrupted_request(CAP)
            result["msg_counts_pre_followup"] = agent_msg_counts(CAP)
            full = open(PTY_LOG, "rb").read()
            tail = clean(full)[-1500:]
            result["pty_has_interrupted_marker"] = any(
                m in tail for m in ("Interrupted", "interrupted", "Stopped", "stopped"))
            result["pty_tail"] = tail
            note("submit follow-up prompt to confirm session is usable")
            send(PROMPT1); time.sleep(0.4); send("\r")
            followed_up = True; last_out = time.time(); interrupt_at = time.time()

        if followed_up and interrupt_at and time.time() - interrupt_at > 18:
            result["msg_counts_post_followup"] = agent_msg_counts(CAP)
            note("exit"); send("\x1b"); time.sleep(0.2)
            send("/exit\r"); time.sleep(0.8); send("\x03"); send("\x04")
            break

    logf.close()
    try:
        os.kill(pid, 15)
    except OSError:
        pass

    print("\n==== INTERRUPT PROBE RESULT ====")
    w = result.get("interrupt_wire", {})
    print(f"interrupted req had message_stop:   {w.get('had_message_stop')}")
    print(f"interrupted req final stop_reason:  {w.get('stop_reason')!r}")
    print(f"interrupted req last SSE events:    {w.get('last_sse_events')}")
    print(f"proxy errors on that req:           {w.get('proxy_errors')}")
    print(f"PTY showed an interrupted marker:   {result.get('pty_has_interrupted_marker')}")
    print(f"agent msg counts pre-followup:      {result.get('msg_counts_pre_followup')}")
    print(f"agent msg counts post-followup:     {result.get('msg_counts_post_followup')} "
          f"(a new entry => session usable after interrupt)")
    with open("/tmp/ao-interrupt-marks.json", "w") as f:
        json.dump(result, f, indent=2, default=str)
    print("markers -> /tmp/ao-interrupt-marks.json")
    print(f"pty log: {PTY_LOG}  capture: {CAP}")


if __name__ == "__main__":
    main()
