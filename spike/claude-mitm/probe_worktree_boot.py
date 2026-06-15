#!/usr/bin/env python3
"""Probe: does the claude-tui composer-ready gate hold on a WORKTREE-SWITCH
resume boot, or does it mis-fire and swallow the first Send?

Bug report: switching a thread to a NEW worktree swallows the first message
(stuck "Working"); the SECOND sends fine. Back-to-main and a normal first start
work. A worktree switch restarts the session with `--resume <id>` IN THE NEW
WORKTREE CWD, where that id was created — so the difference vs the working cases
is "resume a session that belongs to a DIFFERENT cwd."

AO's first-Send gate (claudetui/session_send.go awaitComposerReady):
    ready := ptyBytes >= 512  AND  idle-since-last-PTY-output >= 400ms   (8s timeout)
It latches on the first 400ms-quiet gap after 512 bytes. This spike replicates
that EXACT gate and asks, credit-free, whether the turn actually SUBMITS, across:
  origin-resume  : create session in cwd A, resume in cwd A (≈ back-to-main)   [works?]
  foreign-resume : create session in cwd A, resume in worktree cwd B (the bug) [works?]
  cold-worktree  : no --resume, fresh in worktree cwd B (≈ normal first start) [works?]

Submit detection reuses probe_cold_submit.Mock: ANTHROPIC_BASE_URL → a LOCAL mock
that serves canned end_turn SSE and flags a real submit when POST /v1/messages
carries the run's unique marker. No upstream, no credits. The session is even
SEEDED through the mock, so the whole probe is credit-free. Captures hold only
boot UI; treat the temp config (copied creds) as sensitive and delete after. Run:
    python3 probe_worktree_boot.py
"""
import fcntl
import http.server
import json
import os
import pty
import re
import select
import shutil
import socketserver
import struct
import subprocess
import termios
import threading
import time

import aoprobe
import probe_cold_submit as cs

# Suspected production pause source: real network latency on claude's boot-time
# /v1/messages POST. STARTUP_DELAY models it (the clean mock answers instantly,
# which is why a stripped harness can't reproduce the swallow). Override via env.
STARTUP_DELAY = float(os.environ.get("STARTUP_DELAY", "0.0"))


class SlowMock:
    """probe_cold_submit.Mock, but delays STARTUP /v1/messages POSTs (those whose
    body lacks the run marker) by STARTUP_DELAY — modelling boot-time API latency
    as a >400ms PTY-silent pause after the banner. The SUBMIT POST (carries the
    marker) is answered immediately so submit-timing is unaffected."""

    def __init__(self, startup_delay=0.0):
        self.submit = threading.Event()
        self.markers = []
        self.seen = set()
        self.startup_delay = startup_delay
        self.httpd = None
        self.url = None

    def reset(self, markers):
        self.submit.clear()
        self.markers = list(markers)
        self.seen = set()

    def start(self):
        mock = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _body(self):
                n = int(self.headers.get("Content-Length", 0) or 0)
                return self.rfile.read(n) if n else b""

            def do_POST(self):
                body = self._body()
                if self.path.split("?")[0] == "/v1/messages":
                    matched = [m for m in mock.markers if m in body]
                    if matched:
                        mock.seen.update(matched)
                        mock.submit.set()
                    elif mock.startup_delay > 0:
                        time.sleep(mock.startup_delay)  # model boot-time API latency
                    self.send_response(200)
                    self.send_header("Content-Type", "text/event-stream")
                    self.end_headers()
                    try:
                        self.wfile.write(cs.CANNED_SSE)
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    return
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"input_tokens":1,"output_tokens":1}')

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b"{}")

        self.httpd = socketserver.ThreadingTCPServer(("127.0.0.1", 0), H)
        self.httpd.daemon_threads = True
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        return self.url

    def stop(self):
        if self.httpd:
            self.httpd.shutdown()

MODEL = "claude-opus-4-8"
WT = "/tmp/aowt"                       # the worktree the thread "switches into"
ROWS, COLS = 40, 120

# Production gate constants (session_send.go). Replicated exactly.
MIN_BYTES = 512
QUIET = 0.40
GATE_TIMEOUT = 8.0

# Reliable "composer is up and interactive" markers (bottom shortcut/stat bar).
COMPOSER_MARKERS = [b"forshortcuts", b"shift+tab", b"bypasspermissions",
                    b"foragents", b"esctointerrupt"]
BP_ENABLE = cs.BP_ENABLE              # ESC[?2004h — bracketed paste enabled


def clean_env():
    env = {k: v for k, v in os.environ.items()
           if not (k.startswith("CLAUDE") or k.startswith("ANTHROPIC"))}
    env["CLAUDE_CONFIG_DIR"] = aoprobe.CONFIG_DIR
    env["TERM"] = "xterm-256color"
    return env


def preaccept_and_worktree():
    """Skip the bypass dialog (settings.json skipDangerousModePermissionPrompt +
    .claude.json bypassPermissionsModeAccepted — see probe_cold_submit), make CWD
    a git repo, add a worktree, and trust the worktree path."""
    sp = f"{aoprobe.CONFIG_DIR}/settings.json"
    s = json.load(open(sp))
    s["skipDangerousModePermissionPrompt"] = True
    json.dump(s, open(sp, "w"))

    genv = {"HOME": os.path.expanduser("~"), "PATH": os.environ.get("PATH", "")}
    shutil.rmtree(WT, ignore_errors=True)
    if not os.path.isdir(f"{aoprobe.CWD}/.git"):
        subprocess.run(["git", "init", "-q"], cwd=aoprobe.CWD, env=genv)
        subprocess.run(["git", "config", "user.email", "s@e.com"], cwd=aoprobe.CWD, env=genv)
        subprocess.run(["git", "config", "user.name", "spike"], cwd=aoprobe.CWD, env=genv)
        with open(f"{aoprobe.CWD}/README.md", "w") as f:
            f.write("# spike\n")
        subprocess.run(["git", "add", "-A"], cwd=aoprobe.CWD, env=genv)
        subprocess.run(["git", "commit", "-qm", "init"], cwd=aoprobe.CWD, env=genv)
    # Clean any leftovers from a prior run: the dir, the worktree registration,
    # AND the branch (rmtree alone leaves the last two, so `add -b` would fail).
    subprocess.run(["git", "worktree", "remove", "--force", WT], cwd=aoprobe.CWD, env=genv)
    subprocess.run(["git", "worktree", "prune"], cwd=aoprobe.CWD, env=genv)
    subprocess.run(["git", "branch", "-D", "wtb"], cwd=aoprobe.CWD, env=genv)
    r = subprocess.run(["git", "worktree", "add", "-q", "-b", "wtb", WT],
                       cwd=aoprobe.CWD, env=genv, capture_output=True, text=True)
    if not os.path.isdir(WT):
        raise SystemExit(f"worktree setup failed: {r.stderr.strip()}")

    gp = f"{aoprobe.CONFIG_DIR}/.claude.json"
    g = json.load(open(gp))
    g["bypassPermissionsModeAccepted"] = True
    g.setdefault("projects", {})
    for p in (aoprobe.CWD, WT):
        g["projects"][p] = {"hasTrustDialogAccepted": True,
                            "hasCompletedProjectOnboarding": True,
                            "bypassPermissionsModeAccepted": True,
                            "allowedTools": [], "history": []}
    json.dump(g, open(gp, "w"))


def create_session(mock_url, cwd):
    """Seed a resumable session in cwd via one headless turn through the mock
    (credit-free). Returns its session id."""
    env = clean_env()
    env["ANTHROPIC_BASE_URL"] = mock_url
    r = subprocess.run(["claude", "-p", "seed", "--model", MODEL,
                        "--output-format", "json"], cwd=cwd, env=env,
                       capture_output=True, text=True, timeout=90)
    if r.returncode != 0:
        print(f"  [warn] session seed failed: {r.stderr.strip()[:200]}")
        return None
    try:
        return json.loads(r.stdout).get("session_id")
    except (json.JSONDecodeError, ValueError):
        print(f"  [warn] no session_id in: {r.stdout[:160]}")
        return None


def fork_claude(base_url, cwd, resume_id=None):
    env = clean_env()
    env["ANTHROPIC_BASE_URL"] = base_url
    pid, master = pty.fork()
    if pid == 0:
        os.chdir(cwd)
        argv = ["claude", "--permission-mode", "bypassPermissions",
                "--allow-dangerously-skip-permissions",
                "--thinking-display", "summarized", "--model", MODEL]
        if resume_id:
            argv += ["--resume", resume_id]
        os.execvpe("claude", argv, env)
        os._exit(127)
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    return pid, master


def run_scenario(mock, label, cwd, resume_id, prompt, max_s=20):
    """Fork one claude, apply the PRODUCTION gate, send, report whether it
    submitted (mock) plus the gate-vs-composer-ready timeline."""
    mock.reset(markers=[prompt.encode()])
    pid, master = fork_claude(mock.url, cwd, resume_id)
    raw = bytearray()
    start = last_out = time.time()
    sent_at = latched_ready = None
    bp_at = composer_at = None
    submitted_at = None
    try:
        while time.time() - start < max_s:
            r, _, _ = select.select([master], [], [], 0.05)
            now = time.time()
            if master in r:
                try:
                    data = os.read(master, 65536)
                except OSError:
                    break
                if not data:
                    break
                raw += data
                last_out = now
            norm = aoprobe._norm(bytes(raw))
            if bp_at is None and BP_ENABLE in bytes(raw):
                bp_at = now - start
            if composer_at is None and any(m in norm for m in COMPOSER_MARKERS):
                composer_at = now - start
            # The production gate: ready on (>=512B AND >=400ms idle), else 8s timeout.
            if sent_at is None:
                quiet = now - last_out
                ready = len(raw) >= MIN_BYTES and quiet >= QUIET
                timed_out = (now - start) >= GATE_TIMEOUT
                if ready or timed_out:
                    os.write(master, cs.ao_clear()); time.sleep(cs.SETTLE)
                    os.write(master, cs.ao_paste(prompt)); time.sleep(cs.SETTLE)
                    os.write(master, cs.CR)
                    sent_at = now - start
                    latched_ready = ready  # False ⇒ fired on the 8s timeout
            if mock.submit.is_set():
                submitted_at = time.time() - start
                break
        time.sleep(0.5)
    finally:
        try:
            os.write(master, b"\x1b"); time.sleep(0.1)
            os.write(master, b"\x03\x03"); os.kill(pid, 15)
            time.sleep(0.1); os.kill(pid, 9)
        except OSError:
            pass
        os.close(master)
    norm = aoprobe._norm(bytes(raw))
    return {
        "label": label,
        "submitted": submitted_at is not None,
        "sent_at": round(sent_at, 2) if sent_at else None,
        "latched_ready": latched_ready,        # False ⇒ gate timed out at 8s
        "bp_at": round(bp_at, 2) if bp_at else None,
        "composer_at": round(composer_at, 2) if composer_at else None,
        "text_in_composer": prompt.encode() in norm,
        "tail": norm[-70:].decode("ascii", "replace"),
    }


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    print(f"seeding isolated config + git repo + worktree... "
          f"(startup-POST delay = {STARTUP_DELAY}s)")
    aoprobe.seed_config(events=[])
    preaccept_and_worktree()

    mock = SlowMock(STARTUP_DELAY)
    mock.start()
    print(f"mock at {mock.url}")

    print("seeding a resumable session in the main cwd (through the mock)...")
    sid = create_session(mock.url, aoprobe.CWD)
    print(f"  session id: {sid}\n")

    REPEATS = 3
    scenarios = [
        ("origin-resume", aoprobe.CWD, sid),    # ≈ back-to-main (works per report)
        ("foreign-resume", WT, sid),            # the bug: resume in a foreign cwd
        ("cold-worktree", WT, None),            # ≈ normal first start (works per report)
    ]
    tally = {}
    for label, cwd, rid in scenarios:
        if rid is None and label != "cold-worktree":
            print(f"[skip] {label} — no session id")
            continue
        print(f"=== {label} (cwd={'worktree' if cwd == WT else 'main'}, "
              f"resume={'yes' if rid else 'no'}) ===")
        oks = 0
        for i in range(REPEATS):
            prompt = f"zulu{label.replace('-', '')}{i}mark"
            res = run_scenario(mock, f"{label}-{i}", cwd, rid, prompt)
            oks += 1 if res["submitted"] else 0
            sent, comp = res["sent_at"], res["composer_at"]
            premature = sent is not None and (comp is None or sent < comp)
            print(f"  #{i}: submitted={res['submitted']!s:<5} "
                  f"gate_fired@{sent}s ready={res['latched_ready']} "
                  f"composer@{comp}s {'<<< PREMATURE (gate before composer)' if premature else ''}",
                  flush=True)
            print(f"       text_in_composer={res['text_in_composer']} tail: {res['tail']!r}")
            time.sleep(0.4)
        tally[label] = (oks, REPEATS)

    mock.stop()
    print("\n===== SUMMARY (submitted = POST /v1/messages carried the turn) =====")
    for label, (oks, n) in tally.items():
        print(f"  {label:<16} {oks}/{n} submitted")
    print("\ngate_fired = when awaitComposerReady let the Send through; "
          "ready=False ⇒ it gave up at the 8s timeout.")
    print(f"temp config with creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
