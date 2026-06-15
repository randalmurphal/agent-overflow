#!/usr/bin/env python3
"""Empirically PICK the readiness marker for the claude-tui first-Send fix.

The cold-start harness (probe_cold_submit) reproduces the swallow: sending the
instant the PTY accepts input submits ~0/N. So it can DISCRIMINATE candidate
"claude is now draining stdin" signals — fire the AO send the moment each candidate
first appears on a cold boot and measure submit-rate + fire-time. The EARLIEST
signal that still submits 100% is the safe readiness boundary the fix should gate
on; anything that submits <100% fires too early (same failure as `immediate`).

Candidates:
  immediate     send instantly                         [the bug — baseline, expect low]
  2004h         bracketed-paste-enable ESC[?2004h      [protocol code, version-independent, maybe too early]
  composer      composer bottom-bar text marker        [UI text, content-agnostic, later]
  quiesce-400   init burst + >=400ms idle              [the heuristic prod ships today]
  echo-cr       paste, then gate CR on the text echo   [per-send; note: breaks on multiline paste chips]

Reuses probe_cold_submit's proven machinery unchanged. Credit-free (local mock).
Run:  python3 probe_marker_discriminate.py
"""
import os
import time

import aoprobe
import probe_cold_submit as cs

# A swallow is detectable within a few seconds (claude either submits soon after
# the send or never), so cap each run well below run_scenario's 25s default — a
# non-submitting (swallowed) run otherwise burns the full max_s.
MAX_S = float(os.environ.get("MAX_S", "8"))

# EXACT production marker set — mirrors composerBarMarkers in
# internal/provider/claudetui/composer_ready.go. Chrome-specific (the mode-cycle
# hint and the full-access indicator); production requires ALL of them (AND), so
# strat_composer matches the same way — a single phrase in replayed transcript
# prose won't trip the gate. This run confirms the strings fire on the real binary.
COMPOSER_MARKERS = [b"shift+tabtocycle", b"bypasspermissionson"]


def strat_composer(ctx):
    """Fire the AO send the moment a composer bottom-bar marker first renders."""
    st = ctx["state"]
    if st.get("done"):
        return
    if not all(m in aoprobe._norm(ctx["raw"]) for m in COMPOSER_MARKERS):
        return
    send = ctx["send"]
    send(cs.ao_clear()); time.sleep(cs.SETTLE)
    send(cs.ao_paste(cs.PROMPT)); time.sleep(cs.SETTLE)
    send(cs.CR)
    st["sent_at"] = ctx["since"]
    st["done"] = True


def main():
    aoprobe.seed_config(events=[])
    cs._preaccept_bypass()
    mock = cs.Mock()
    mock.url = mock.start()
    print(f"mock {mock.url}\n")
    REPEATS = 5
    # immediate (5/5 swallowed, text lands) and 2004h (4/4 swallowed, text lost —
    # bracketed-paste-enable fires too early) are already conclusive from the prior
    # run; this pass measures the three viable candidates.
    scenarios = [
        ("composer", strat_composer),
        ("quiesce-400", cs.make_quiesce(0.40)),
        ("echo-cr", cs.strat_echo_cr),
    ]
    tally = {}
    for label, strat in scenarios:
        oks, sent_times = 0, []
        for i in range(REPEATS):
            res = cs.run_scenario(mock, f"disc-{label}-{i}", strat, max_s=MAX_S)
            oks += 1 if res["submitted"] else 0
            if res["sent_at_s"]:
                sent_times.append(res["sent_at_s"])
            print(f"  {label:<12}#{i}: submitted={res['submitted']!s:<5} "
                  f"sent@{res['sent_at_s']}s submit@{res['submit_s']}s "
                  f"text={res['text_seen']}", flush=True)
            time.sleep(0.4)
        avg = round(sum(sent_times) / len(sent_times), 2) if sent_times else None
        tally[label] = (oks, REPEATS, avg)
    mock.stop()
    print("\n===== submit rate by readiness marker (cold boot, N=5) =====")
    for label, (oks, n, avg) in tally.items():
        print(f"  {label:<12} {oks}/{n} submitted   avg fire@{avg}s")
    print(f"\ntemp creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
