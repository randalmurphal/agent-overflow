#!/usr/bin/env python3
"""Last untested real-world factor: a BIG transcript (user was mid-conversation),
whose resume-replay may stream with mid-replay >400ms gaps AFTER crossing 512 bytes
but BEFORE the composer mounts → the gate fires mid-replay → first Send swallowed.

Every prior probe seeded a 1-turn session, so the replay was a single quick burst
with no gap and the gate's byte-threshold stayed coupled to the composer paint. The
real worktree switch resumes a session the user had been working in — many turns,
big messages. This seeds a multi-turn transcript (credit-free, via repeated
`claude -p --resume` through the mock), then interactively FOREIGN-resumes it in a
big worktree and checks whether the replay introduces a pre-composer gap the gate
mis-fires into.

submitted=False (or gate_fired before composer) ⇒ reproduced.

Run:  TURNS=8 python3 probe_big_transcript.py
"""
import os
import subprocess
import time

import aoprobe
import probe_bigrepo_boot as big
import probe_foreign_resume_bigrepo as fr
import probe_worktree_boot as wb

TURNS = int(os.environ.get("TURNS", "8"))
# A chunky per-turn prompt so each replayed user message is substantial.
TURN_PROMPT = ("paragraph of conversation content to bulk the transcript so the "
               "resume replay is large and streams over several PTY chunks. " * 12).strip()


def seed_multi_turn(mock_url, cwd, turns):
    """Build a multi-turn transcript by resuming a headless session repeatedly
    through the mock (credit-free). Returns the session id."""
    env = wb.clean_env()
    env["ANTHROPIC_BASE_URL"] = mock_url
    sid = None
    for t in range(turns):
        argv = ["claude", "-p", f"turn {t}: {TURN_PROMPT}", "--model", wb.MODEL,
                "--output-format", "json"]
        if sid:
            argv += ["--resume", sid]
        r = subprocess.run(argv, cwd=cwd, env=env, capture_output=True,
                           text=True, timeout=90)
        if r.returncode != 0:
            raise SystemExit(f"seed turn {t} failed: {r.stderr.strip()[:200]}")
        import json
        sid = json.loads(r.stdout).get("session_id") or sid
    return sid


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    aoprobe.seed_config(events=[])
    big.make_big_worktree()
    big.trust(big.BIGWT)
    big.trust(big.REPO)
    mock = wb.SlowMock(0.0)
    mock.start()
    print(f"mock {mock.url}")
    print(f"seeding a {TURNS}-turn transcript in {big.REPO} ...")
    sid = seed_multi_turn(mock.url, big.REPO, TURNS)
    print(f"  id: {sid}; resuming FOREIGN in {big.BIGWT}\n")
    try:
        oks = 0
        for i in range(4):
            prompt = f"zulubigtx{i}mark"
            res = fr.boot_resume(big.BIGWT, sid, prompt, mock, max_s=22)
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
                  f"{sorted(pre, key=lambda x: -x[1])[:6]}")
            time.sleep(0.4)
        print(f"\n{oks}/4 submitted "
              f"({'REPRODUCED swallow' if oks < 4 else 'still no swallow'})")
    finally:
        mock.stop()
        big.cleanup_big_worktree()
    print(f"temp creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
