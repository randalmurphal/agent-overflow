#!/usr/bin/env python3
"""Probe Claude Code's Remote Control as a third-party local client.

Validates the two open questions from FINDINGS §10:
  (1) Is the daemon `control.sock` RPC speakable by a non-Anthropic local client?
  (2) What does the structured bridge channel look like (see bridge_logger.py)?

Brings up `claude remote-control` under a PTY with LOCAL_BRIDGE=1 (so the bridge
dials our local logger on :8765 instead of the cloud), waits for the daemon to
create its Unix control socket, then connects to it directly and tries to speak
the protocol (`ping`, `list`, `leases`) under both plausible framings.
"""
import glob
import json
import os
import pty
import select
import socket
import struct
import sys
import termios
import fcntl
import time

UID = os.getuid()
SOCK_GLOB = f"/tmp/cc-daemon-{UID}/*/control.sock"
PIPE_KEY = os.path.expanduser("~/.claude/daemon/pipe.key")
PTY_LOG = "/tmp/rc-pty.log"
PROBE_LOG = "/tmp/rc-control-probe.log"
DEBUG_FILE = "/tmp/rc-debug.log"
SPIKE_DIR = "/home/rmurphy/repos/agent-overflow-claude-mitm-spike"

probe = open(PROBE_LOG, "w")


def log(msg):
    line = f"[{time.time():.3f}] {msg}\n"
    sys.stderr.write(line); sys.stderr.flush()
    probe.write(line); probe.flush()


def hexdump(b, n=256):
    b = b[:n]
    out = []
    for i in range(0, len(b), 16):
        c = b[i:i + 16]
        out.append(f"    {i:04x}  " + " ".join(f"{x:02x}" for x in c).ljust(48)
                   + " " + "".join(chr(x) if 32 <= x < 127 else "." for x in c))
    return "\n".join(out)


def recv_some(s, timeout=2.0):
    s.settimeout(timeout)
    try:
        return s.recv(65536)
    except socket.timeout:
        return b""
    except OSError as e:
        return f"<error {e}>".encode()


def try_framing_ndjson(path, op):
    """Newline-delimited JSON: {"proto":1,"op":...}\\n"""
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        s.connect(path)
    except OSError as e:
        log(f"  NDJSON connect failed: {e}")
        return
    greeting = recv_some(s, 0.5)
    if greeting:
        log(f"  NDJSON on-connect greeting ({len(greeting)}B):\n{hexdump(greeting)}")
    req = (json.dumps({"proto": 1, "op": op}) + "\n").encode()
    s.sendall(req)
    resp = recv_some(s, 2.0)
    log(f"  NDJSON op={op!r}: sent {req!r} -> resp ({len(resp)}B):\n{hexdump(resp)}")
    s.close()


def try_framing_len32(path, op):
    """4-byte little-endian length prefix + JSON payload."""
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        s.connect(path)
    except OSError as e:
        log(f"  LEN32 connect failed: {e}")
        return
    greeting = recv_some(s, 0.5)
    if greeting:
        log(f"  LEN32 on-connect greeting ({len(greeting)}B):\n{hexdump(greeting)}")
    payload = json.dumps({"proto": 1, "op": op}).encode()
    s.sendall(struct.pack("<I", len(payload)) + payload)
    resp = recv_some(s, 2.0)
    log(f"  LEN32 op={op!r}: sent len={len(payload)} -> resp ({len(resp)}B):\n{hexdump(resp)}")
    s.close()


def probe_control_sock():
    socks = glob.glob(SOCK_GLOB)
    log(f"control.sock glob {SOCK_GLOB} -> {socks}")
    if os.path.exists(PIPE_KEY):
        log(f"pipe.key present: {open(PIPE_KEY).read().strip()!r} "
            f"(mode {oct(os.stat(PIPE_KEY).st_mode)})")
    else:
        log("pipe.key absent")
    if not socks:
        log("NO control.sock found — daemon did not expose a local socket")
        return
    path = socks[0]
    st = os.stat(path)
    log(f"socket {path} mode={oct(st.st_mode)} uid={st.st_uid}")
    for op in ("ping", "list", "leases"):
        log(f"-- probing op={op!r} --")
        try_framing_ndjson(path, op)
        try_framing_len32(path, op)


def set_winsize(fd, rows=50, cols=200):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def main():
    env = dict(os.environ)
    env["LOCAL_BRIDGE"] = "1"            # dial ws://localhost:8765, not the cloud
    env["TERM"] = "xterm-256color"
    env["CLAUDE_REMOTE_CONTROL_SESSION_NAME_PREFIX"] = "spike"
    os.chdir(SPIKE_DIR)

    for f in (PTY_LOG, DEBUG_FILE):
        try:
            os.remove(f)
        except OSError:
            pass

    pid, master = pty.fork()
    if pid == 0:
        os.execvpe("claude",
                   ["claude", "remote-control", "--spawn", "same-dir",
                    "--debug-file", DEBUG_FILE, "--verbose"],
                   env)
        os._exit(127)
    set_winsize(master)

    log(f"launched `claude remote-control` pid={pid} (LOCAL_BRIDGE=1)")
    ptylog = open(PTY_LOG, "wb")
    start = time.time()
    probed = False
    enabled_at = None
    spawn_at = None
    seen_spawn_prompt = False
    tail = b""
    while time.time() - start < 34:
        r, _, _ = select.select([master], [], [], 0.5)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            ptylog.write(data); ptylog.flush()
            tail = (tail + data)[-4000:]
            if enabled_at is None and b"Enable Remote Control?" in tail:
                log("prompt 'Enable Remote Control?' -> 'y'")
                os.write(master, b"y\r"); enabled_at = time.time(); tail = b""
            elif spawn_at is None and (b"Choose [1/2]" in tail or b"Spawn mode for this project" in tail):
                seen_spawn_prompt = True
                log("prompt spawn-mode -> '1' (same-dir)")
                os.write(master, b"1\r"); spawn_at = time.time(); tail = b""
        # Probe once the daemon has had time to spin up after the last prompt.
        proceed = ((spawn_at and time.time() - spawn_at > 7)
                   or (enabled_at and not seen_spawn_prompt and time.time() - enabled_at > 9)
                   or time.time() - start > 24)
        if not probed and proceed:
            log("=== probing control.sock ===")
            probe_control_sock()
            probed = True
    ptylog.close()

    log("tearing down")
    os.write(master, b"\x03")  # ctrl-c (RC says "Press Ctrl+C to stop")
    time.sleep(0.4)
    os.write(master, b"\x03")
    time.sleep(0.5)
    try:
        os.kill(pid, 15)
    except OSError:
        pass
    os.system("claude daemon stop --any >/dev/null 2>&1")
    log(f"done. pty={PTY_LOG} probe={PROBE_LOG} debug={DEBUG_FILE} bridge=/tmp/rc-bridge.log")


if __name__ == "__main__":
    main()
