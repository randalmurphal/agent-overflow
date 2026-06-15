#!/usr/bin/env python3
"""Reproduce the claude-tui worktree first-message swallow by manufacturing the
pre-composer PTY-silent pause the clean harness lacks.

Hypothesis: a slow-to-initialize MCP server (modelling dispatch-atlassian /
dispatch-gitlab) makes claude's TUI boot go quiet for >400ms AFTER the banner
(>=512 bytes) but BEFORE the composer is interactive. AO's gate then latches in
that window and writes paste+Enter while claude isn't draining stdin → the Enter
is swallowed → the first message never submits.

Reuses probe_worktree_boot's machinery (exact production gate, credit-free submit
detection via SlowMock, worktree setup). The ONLY addition is a slow MCP server
in the seeded config. Reads per scenario:
  - composer@   : when the composer marker rendered (delayed by MCP ⇒ blocks boot)
  - gate_fired@ : when awaitComposerReady let the Send through
  - PREMATURE   : gate fired before the composer was ready
  - submitted   : did POST /v1/messages carry the turn (False ⇒ swallowed)

Run:  MCP_INIT_SLEEP=2.0 python3 probe_worktree_repro.py
"""
import os
import time

import aoprobe
import probe_worktree_boot as wb

HERE = os.path.dirname(os.path.abspath(__file__))
SLOW_MCP = os.path.join(HERE, "slow_mcp.py")
INIT_SLEEP = os.environ.get("MCP_INIT_SLEEP", "2.0")
BLOCK_ON = os.environ.get("MCP_BLOCK_ON", "init")


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    mcp = {"slowmcp": {"type": "stdio", "command": "python3", "args": [SLOW_MCP],
                       "env": {"MCP_INIT_SLEEP": INIT_SLEEP, "MCP_BLOCK_ON": BLOCK_ON}}}
    print(f"seeding config with a slow MCP (sleep={INIT_SLEEP}s on {BLOCK_ON}) + worktree...")
    aoprobe.seed_config(events=[], mcp_servers=mcp)
    wb.preaccept_and_worktree()

    mock = wb.SlowMock(0.0)   # pause comes from MCP, not the mock
    mock.start()
    print(f"mock at {mock.url}")
    sid = wb.create_session(mock.url, aoprobe.CWD)
    print(f"session id: {sid}\n")

    scenarios = [
        ("resume-worktree", wb.WT, sid),
        ("cold-worktree", wb.WT, None),
        ("origin-resume", aoprobe.CWD, sid),
    ]
    tally = {}
    for label, cwd, rid in scenarios:
        print(f"=== {label} (slow MCP active) ===")
        oks = 0
        for i in range(3):
            prompt = f"zulu{label.replace('-', '')}{i}mark"
            res = wb.run_scenario(mock, f"{label}-{i}", cwd, rid, prompt, max_s=16)
            oks += 1 if res["submitted"] else 0
            sent, comp = res["sent_at"], res["composer_at"]
            premature = sent is not None and (comp is None or sent < comp)
            print(f"  #{i}: submitted={res['submitted']!s:<5} gate_fired@{sent}s "
                  f"composer@{comp}s ready={res['latched_ready']} "
                  f"{'<<< PREMATURE (gate before composer)' if premature else ''}")
            print(f"       text_in_composer={res['text_in_composer']} tail: {res['tail']!r}")
            time.sleep(0.4)
        tally[label] = (oks, 3)

    mock.stop()
    print("\n===== SUMMARY (submitted = POST /v1/messages carried the turn) =====")
    for label, (oks, n) in tally.items():
        verdict = "  <-- REPRODUCED swallow" if oks < n else ""
        print(f"  {label:<18} {oks}/{n} submitted{verdict}")
    print(f"\ntemp config with creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
