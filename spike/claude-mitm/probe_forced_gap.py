#!/usr/bin/env python3
"""Force claude's cwd-dependent boot work past the gate's 400ms threshold to get a
DETERMINISTIC red (submitted=False), confirming the swallow mechanism end-to-end.

probe_bigrepo_boot saw the pre-composer gap as cwd-dependent startup work: in its
run #0 claude emitted ~87 bytes, went PTY-silent for 478ms, THEN dumped the
banner+composer — i.e. it analyzes the cwd BEFORE painting. A tiny cwd → no gap; a
big cwd → a ~0.3-0.48s gap that hovers at the 400ms cliff. This probe enlarges that
synchronous startup work — a big project CLAUDE.md claude reads at boot + many
untracked files its boot-time `git status` must stat — so the gap reliably exceeds
400ms. Combined with resume replay (which front-loads >512 bytes before the gap),
the gate latches mid-gap and writes paste+CR before the composer mounts → the CR is
swallowed → submitted=False.

If this reproduces the swallow it both (a) proves the mechanism end-to-end and (b)
identifies the lever (synchronous cwd startup work). Credit-free via SlowMock.

Run:  python3 probe_forced_gap.py
"""
import os
import subprocess
import time

import aoprobe
import probe_bigrepo_boot as big
import probe_foreign_resume_bigrepo as fr
import probe_worktree_boot as wb

NFILES = int(os.environ.get("NFILES", "40000"))
CLAUDEMD_KB = int(os.environ.get("CLAUDEMD_KB", "400"))


def bloat_worktree():
    """Add synchronous boot work to BIGWT: a big project CLAUDE.md (read at boot)
    and many untracked files (boot-time git status must stat them)."""
    md = ("# Project\n\n" + ("guidance paragraph repeated to bulk the file. " * 20
                             + "\n") * (CLAUDEMD_KB * 1024 // 940))
    with open(f"{big.BIGWT}/CLAUDE.md", "w") as f:
        f.write(md)
    # Many untracked files across nested dirs to slow boot-time `git status`.
    per_dir = 400
    for d in range(NFILES // per_dir):
        dp = f"{big.BIGWT}/bloat/d{d:04d}"
        os.makedirs(dp, exist_ok=True)
        for i in range(per_dir):
            with open(f"{dp}/f{i:03d}.txt", "w") as f:
                f.write("x")
    # Confirm git status is actually slow now (proxy for claude's boot-time stat).
    genv = {"HOME": os.path.expanduser("~"), "PATH": os.environ.get("PATH", "")}
    t = time.time()
    subprocess.run(["git", "status", "--porcelain"], cwd=big.BIGWT, env=genv,
                   capture_output=True)
    print(f"bloated: CLAUDE.md={len(md)//1024}KB, ~{NFILES} untracked files; "
          f"git status now {round(time.time() - t, 2)}s")


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    aoprobe.seed_config(events=[])
    big.make_big_worktree()
    bloat_worktree()
    big.trust(big.BIGWT)
    big.trust(big.REPO)
    mock = wb.SlowMock(0.0)
    mock.start()
    print(f"mock {mock.url}")
    sid = fr.seed_session(mock.url, big.REPO)
    print(f"session seeded in {big.REPO}; resuming FOREIGN in bloated {big.BIGWT}\n")
    try:
        oks = 0
        for i in range(4):
            prompt = f"zuluforced{i}mark"
            res = fr.boot_resume(big.BIGWT, sid, prompt, mock, max_s=20)
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
              f"({'REPRODUCED swallow' if oks < 4 else 'still no swallow'})")
    finally:
        mock.stop()
        big.cleanup_big_worktree()
    print(f"temp creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
