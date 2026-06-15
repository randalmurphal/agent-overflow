#!/usr/bin/env python3
"""Does a slow SessionStart HOOK delay claude's composer-render (the pre-composer
pause that would mis-fire AO's first-Send gate)?

AO registers a SessionStart hook (`agent-overflow __claude-hook`) — claude runs
it during boot, and on a worktree switch it cold-starts the AO binary. If claude
blocks composer-render on that hook, a slow hook is the missing pre-composer
pause the clean harness lacks. This seeds a SessionStart hook that just sleeps,
then measures when the composer renders vs when AO's gate fires.

Run:  HOOK_SLEEP=2.0 python3 probe_boot_pause.py
"""
import json
import os
import time

import aoprobe
import probe_worktree_boot as wb

HOOK_SLEEP = float(os.environ.get("HOOK_SLEEP", "2.0"))


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    aoprobe.seed_config(events=[])
    wb.preaccept_and_worktree()
    # Overwrite settings.json with a SessionStart hook that simply sleeps —
    # models AO's __claude-hook taking a beat to cold-start the binary on boot.
    sp = f"{aoprobe.CONFIG_DIR}/settings.json"
    s = json.load(open(sp))
    s.setdefault("hooks", {})["SessionStart"] = [{"hooks": [{
        "type": "command",
        "command": f'python3 -c "import time; time.sleep({HOOK_SLEEP})"',
    }]}]
    json.dump(s, open(sp, "w"))

    mock = wb.SlowMock(0.0)
    mock.start()
    print(f"slow SessionStart hook = {HOOK_SLEEP}s; mock at {mock.url}")
    print("(composer@ much later than ~0.6s ⇒ the hook blocks the boot)\n")

    oks = 0
    for i in range(3):
        prompt = f"zuluhook{i}mark"
        res = wb.run_scenario(mock, f"hook-{i}", wb.WT, None, prompt, max_s=16)
        oks += 1 if res["submitted"] else 0
        sent, comp = res["sent_at"], res["composer_at"]
        premature = sent is not None and (comp is None or sent < comp)
        print(f"  #{i}: submitted={res['submitted']!s:<5} gate_fired@{sent}s "
              f"composer@{comp}s ready={res['latched_ready']} "
              f"{'<<< PREMATURE (gate before composer) — SWALLOW' if premature else ''}")
        print(f"       text_in_composer={res['text_in_composer']} tail: {res['tail']!r}")
        time.sleep(0.4)
    mock.stop()
    print(f"\n{oks}/3 submitted "
          f"({'REPRODUCED swallow' if oks < 3 else 'no swallow — hook did not block'})")
    print(f"temp creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
