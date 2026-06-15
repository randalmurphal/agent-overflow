#!/usr/bin/env python3
"""DETERMINISTIC repro of the worktree first-message swallow: resume a session
(front-loads >512 replay bytes early) in a REAL big-repo worktree (whose
cwd-dependent boot work goes PTY-silent for >400ms before the composer).

probe_bigrepo_boot.py already OBSERVED a real 478ms pre-composer gap booting in a
detached worktree of agent-overflow — the exact pause AO's idle-gate
(ptyReadyForSend: >=512 bytes AND >=400ms idle) mis-fires into. That cold run
didn't swallow only because <512 bytes had arrived before the gap, so the gate's
byte-threshold held by luck. Production loses that race because a worktree switch
restarts with `--resume <turn-1 id>`: the transcript REPLAY dumps >512 bytes right
after the banner, BEFORE the cwd-work gap. So the gate sees >=512 bytes, then the
>400ms gap → it latches mid-gap and writes clear+paste+CR while claude is still
doing cwd work (composer not yet interactive) → the submit CR is swallowed.

This reproduces that exact combination, credit-free (SlowMock), and reports whether
the turn SUBMITS. submitted=False ⇒ the swallow is reproduced.

Run:  python3 probe_resume_bigrepo.py
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
import probe_bigrepo_boot as big
import probe_worktree_boot as wb

# A long seed prompt so the resume REPLAY front-loads well past 512 bytes in the
# first burst, before claude's cwd-dependent boot work goes quiet.
SEED_PROMPT = ("context seed paragraph. " * 90).strip()


def seed_session(mock_url, cwd):
    """One headless turn through the mock (credit-free) to mint a resumable
    session whose transcript is big enough that resume-replay exceeds 512 bytes
    early. Returns the session id."""
    env = wb.clean_env()
    env["ANTHROPIC_BASE_URL"] = mock_url
    r = subprocess.run(["claude", "-p", SEED_PROMPT, "--model", wb.MODEL,
                        "--output-format", "json"], cwd=cwd, env=env,
                       capture_output=True, text=True, timeout=90)
    if r.returncode != 0:
        raise SystemExit(f"seed failed: {r.stderr.strip()[:200]}")
    return json.loads(r.stdout).get("session_id")


def boot_resume(cwd, resume_id, prompt, mock, max_s=18):
    """Boot `claude --resume <id>` in cwd, record the boot timeline, apply the
    EXACT production gate, report whether the turn submitted."""
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
    sent_at = composer_at = latched_ready = None
    bytes_at_gate = None
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
            "bytes_at_gate": bytes_at_gate, "timeline": timeline}


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    aoprobe.seed_config(events=[])
    big.make_big_worktree()
    big.trust(big.BIGWT)
    mock = wb.SlowMock(0.0)
    mock.start()
    print(f"mock {mock.url}")
    sid = seed_session(mock.url, big.BIGWT)
    print(f"resumable session (long transcript) id: {sid}\n")
    try:
        oks = 0
        for i in range(4):
            prompt = f"zuluresumebig{i}mark"
            res = boot_resume(big.BIGWT, sid, prompt, mock)
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
            time.sleep(0.4)
        print(f"\n{oks}/4 submitted "
              f"({'REPRODUCED swallow' if oks < 4 else 'no swallow this run'})")
    finally:
        mock.stop()
        big.cleanup_big_worktree()
    print(f"temp creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
