#!/usr/bin/env python3
"""The faithful repro: resume a session in a FOREIGN big-repo worktree (the one
condition that distinguishes the failing case from the working ones).

Per the bug report, these WORK: a normal first start (cold, no resume) and
back-to-main (resume in the session's ORIGIN cwd). Only this FAILS: switching to a
NEW worktree, which restarts with `claude --resume <turn-1 id>` in a cwd DIFFERENT
from where that session was created. So the distinguishing factor is cross-cwd
resume, and the real worktree is a big checkout (where probe_bigrepo_boot observed
a real 478ms pre-composer gap). This probe combines both: it seeds a session in cwd
A (the repo root) and resumes it in cwd B (a detached worktree), both big, with the
EXACT production gate — and reports, credit-free, whether the turn submits and what
the boot screen shows (a cross-cwd resume could replay, prompt, or stall).

Two questions answered at once:
  1. Does `claude --resume <id>` even work from a FOREIGN cwd, or show a
     picker/error the gate would mis-fire into? (binary behavior — verify, don't
     guess.)
  2. Does cross-cwd resume in a big repo produce the >400ms pre-composer gap that
     swallows the first Send? (submitted=False ⇒ reproduced.)

Run:  python3 probe_foreign_resume_bigrepo.py
"""
import fcntl
import json
import os
import pty
import re
import select
import struct
import subprocess
import termios
import time

import aoprobe
import probe_bigrepo_boot as big
import probe_worktree_boot as wb

ANSI = re.compile(rb"\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)"
                  rb"|\x1b[()][AB012]|\x1b[=>]|[\x00-\x08\x0b-\x1f]")
SEED_PROMPT = ("context seed paragraph for a resumable transcript. " * 60).strip()


def deansi(raw):
    return re.sub(r"\n{3,}", "\n\n",
                  ANSI.sub(b"", raw.replace(b"\r", b"")).decode("utf-8", "replace"))


def seed_session(mock_url, cwd):
    env = wb.clean_env()
    env["ANTHROPIC_BASE_URL"] = mock_url
    r = subprocess.run(["claude", "-p", SEED_PROMPT, "--model", wb.MODEL,
                        "--output-format", "json"], cwd=cwd, env=env,
                       capture_output=True, text=True, timeout=90)
    if r.returncode != 0:
        raise SystemExit(f"seed failed: {r.stderr.strip()[:200]}")
    return json.loads(r.stdout).get("session_id")


def boot_resume(cwd, resume_id, prompt, mock, max_s=18):
    mock.reset(markers=[prompt.encode()])
    env = wb.clean_env()
    env["ANTHROPIC_BASE_URL"] = mock.url
    pid, master = pty.fork()
    if pid == 0:
        os.chdir(cwd)
        os.execvpe("claude", ["claude", "--resume", resume_id,
                              "--permission-mode", "bypassPermissions",
                              "--allow-dangerously-skip-permissions",
                              "--thinking-display", "summarized",
                              "--model", wb.MODEL], env)
        os._exit(127)
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
    raw, start, last = bytearray(), time.time(), time.time()
    sent_at = composer_at = latched_ready = bytes_at_gate = None
    timeline = []
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
                bytes_at_gate = len(raw)
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
            "bytes_at_gate": bytes_at_gate, "timeline": timeline,
            "screen": deansi(bytes(raw))}


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    aoprobe.seed_config(events=[])
    big.make_big_worktree()
    big.trust(big.BIGWT)
    big.trust(big.REPO)  # the session's ORIGIN cwd must be trusted too
    mock = wb.SlowMock(0.0)
    mock.start()
    print(f"mock {mock.url}")
    sid = seed_session(mock.url, big.REPO)        # cwd A = repo root
    print(f"session seeded in ORIGIN cwd {big.REPO}\n  id: {sid}")
    print(f"resuming in FOREIGN cwd {big.BIGWT}\n")
    try:
        oks = 0
        for i in range(3):
            prompt = f"zuluforeign{i}mark"
            res = boot_resume(big.BIGWT, sid, prompt, mock)   # cwd B = worktree
            oks += 1 if res["submitted"] else 0
            sent, comp = res["sent_at"], res["composer_at"]
            premature = sent is not None and (comp is None or sent < comp)
            print(f"#{i}: submitted={res['submitted']} "
                  f"gate@{round(sent, 2) if sent else None}s "
                  f"(bytes={res['bytes_at_gate']}) "
                  f"composer@{round(comp, 2) if comp else None}s "
                  f"ready={res['latched_ready']} "
                  f"{'<<< PREMATURE — SWALLOW' if premature else ''}")
            tl = res["timeline"]
            pre = [(t, g, n) for (t, g, n) in tl if comp is None or t <= comp]
            print(f"    biggest pre-composer gaps: "
                  f"{sorted(pre, key=lambda x: -x[1])[:5]}")
            if i == 0:
                print("    --- boot screen (last 1100 chars) ---")
                print("    " + res["screen"][-1100:].replace("\n", "\n    "))
            time.sleep(0.4)
        print(f"\n{oks}/3 submitted "
              f"({'REPRODUCED swallow' if oks < 3 else 'no swallow this run'})")
    finally:
        mock.stop()
        big.cleanup_big_worktree()
    print(f"temp creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
