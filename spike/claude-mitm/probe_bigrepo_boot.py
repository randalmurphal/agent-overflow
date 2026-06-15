#!/usr/bin/env python3
"""Does a REAL big-repo worktree cwd create a pre-composer >400ms gap in claude's
boot output that AO's idle-gate fires into (the first-message swallow)?

Every clean spike booted in a tiny empty cwd (/tmp/aowt); the composer rendered at
~0.6s with no gap, so the gate never mis-fired. The real worktree a thread switches
into is a FULL checkout of agent-overflow: a big CLAUDE.md/AGENTS.md tree claude
reads at boot and a large git working tree it runs `git status` over. That
synchronous, cwd-dependent boot work is exactly what ptyReadyForSend's idle
heuristic is blind to — if claude goes PTY-silent for >400ms AFTER the >=512-byte
banner but BEFORE the composer is interactive, the gate latches and writes
paste+Enter into a claude not yet draining stdin → the submit CR is swallowed.

This boots claude in a DETACHED worktree of the real agent-overflow repo (faithful
to production: full tracked tree, clean status, real CLAUDE.md files), records the
inter-chunk boot timeline so a pre-composer gap is visible, applies the EXACT
production gate, and reports — credit-free via SlowMock — whether the turn submits.

Run:  python3 probe_bigrepo_boot.py
"""
import fcntl
import json
import os
import pty
import select
import struct
import subprocess
import termios
import time

import aoprobe
import probe_worktree_boot as wb

REPO = "/home/rmurphy/repos/agent-overflow"
BIGWT = "/tmp/ao-bigwt"
GENV = {"HOME": os.path.expanduser("~"), "PATH": os.environ.get("PATH", "")}


def make_big_worktree():
    subprocess.run(["git", "worktree", "remove", "--force", BIGWT], cwd=REPO,
                   env=GENV, capture_output=True)
    subprocess.run(["git", "worktree", "prune"], cwd=REPO, env=GENV, capture_output=True)
    r = subprocess.run(["git", "worktree", "add", "-q", "--detach", BIGWT],
                       cwd=REPO, env=GENV, capture_output=True, text=True)
    if not os.path.isdir(BIGWT):
        raise SystemExit(f"big worktree setup failed: {r.stderr.strip()}")
    n = sum(len(files) for _, _, files in os.walk(BIGWT) if "/.git" not in _)
    print(f"detached worktree of real repo at {BIGWT} ({n} tracked-tree files)")


def cleanup_big_worktree():
    subprocess.run(["git", "worktree", "remove", "--force", BIGWT], cwd=REPO,
                   env=GENV, capture_output=True)
    subprocess.run(["git", "worktree", "prune"], cwd=REPO, env=GENV, capture_output=True)


def trust(path):
    sp = f"{aoprobe.CONFIG_DIR}/settings.json"
    s = json.load(open(sp))
    s["skipDangerousModePermissionPrompt"] = True
    json.dump(s, open(sp, "w"))
    gp = f"{aoprobe.CONFIG_DIR}/.claude.json"
    g = json.load(open(gp))
    g["bypassPermissionsModeAccepted"] = True
    g.setdefault("projects", {})[path] = {
        "hasTrustDialogAccepted": True, "hasCompletedProjectOnboarding": True,
        "bypassPermissionsModeAccepted": True, "allowedTools": [], "history": []}
    json.dump(g, open(gp, "w"))


def boot_timeline(cwd, prompt, mock, max_s=16):
    """Boot claude in cwd, record the per-read boot timeline (so a pre-composer
    PTY-silent gap is visible), and apply the EXACT production gate."""
    mock.reset(markers=[prompt.encode()])
    env = wb.clean_env()
    env["ANTHROPIC_BASE_URL"] = mock.url
    pid, master = pty.fork()
    if pid == 0:
        os.chdir(cwd)
        os.execvpe("claude", ["claude", "--permission-mode", "bypassPermissions",
                              "--allow-dangerously-skip-permissions",
                              "--thinking-display", "summarized",
                              "--model", wb.MODEL], env)
        os._exit(127)
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
    raw, start, last = bytearray(), time.time(), time.time()
    sent_at = composer_at = latched_ready = None
    timeline = []  # (t_since_start, gap_since_last_data, nbytes) per data read
    while time.time() - start < max_s:
        r, _, _ = select.select([master], [], [], 0.02)
        now = time.time()
        if master in r:
            try:
                d = os.read(master, 65536)
            except OSError:
                break
            if not d:
                break
            timeline.append((round(now - start, 3), round(now - last, 3), len(d)))
            raw += d
            last = now
        norm = aoprobe._norm(bytes(raw))
        if composer_at is None and any(m in norm for m in wb.COMPOSER_MARKERS):
            composer_at = now - start
        if sent_at is None:
            ready = len(raw) >= wb.MIN_BYTES and (now - last) >= wb.QUIET
            if ready or (now - start) >= wb.GATE_TIMEOUT:
                os.write(master, wb.cs.ao_clear()); time.sleep(wb.cs.SETTLE)
                os.write(master, wb.cs.ao_paste(prompt)); time.sleep(wb.cs.SETTLE)
                os.write(master, wb.cs.CR)
                sent_at, latched_ready = now - start, ready
        if mock.submit.is_set():
            break
    time.sleep(0.3)
    for sig in (15, 9):
        try:
            os.kill(pid, sig)
        except OSError:
            break
        time.sleep(0.1)
    os.close(master)
    return {"submitted": mock.submit.is_set(), "sent_at": sent_at,
            "composer_at": composer_at, "latched_ready": latched_ready,
            "timeline": timeline}


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    aoprobe.seed_config(events=[])
    make_big_worktree()
    trust(BIGWT)
    mock = wb.SlowMock(0.0)
    mock.start()
    print(f"mock {mock.url}\n")
    try:
        for i in range(3):
            prompt = f"zulubigrepo{i}mark"
            res = boot_timeline(BIGWT, prompt, mock)
            sent, comp = res["sent_at"], res["composer_at"]
            premature = sent is not None and (comp is None or sent < comp)
            print(f"#{i}: submitted={res['submitted']} "
                  f"gate@{round(sent, 2) if sent else None}s "
                  f"composer@{round(comp, 2) if comp else None}s "
                  f"ready={res['latched_ready']} "
                  f"{'<<< PREMATURE — SWALLOW' if premature else ''}")
            tl = res["timeline"]
            pre = [(t, g, n) for (t, g, n) in tl if comp is None or t <= comp]
            biggest = sorted(pre, key=lambda x: -x[1])[:5]
            print(f"    biggest pre-composer gaps (t, gap, bytes): {biggest}")
            time.sleep(0.4)
    finally:
        mock.stop()
        cleanup_big_worktree()
    print(f"\ntemp creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
