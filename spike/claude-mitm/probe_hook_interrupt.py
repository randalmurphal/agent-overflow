#!/usr/bin/env python3
"""Probe: interrupt (Esc) during TOOL EXECUTION — the case the original spike
only had from source. Confirms the observable signals AO would finalize on:
  - PostToolUse tool_response.interrupted == true (a hook signal!), and/or
  - the synthetic "[Request interrupted by user...]" marker in the transcript,
  - the foreground tool's side effect did NOT complete (process was reaped).

Runs a foreground slow Bash, waits for it to start (PreToolUse Bash), sends a
single Esc, then inspects hook payloads + transcript.
"""
import json
import os
import time

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
LATE = "/tmp/aohook/late.txt"
FIFO = "/tmp/aohook/fifo"
PTY_LOG = "/tmp/aohook/pty-interrupt.log"
# A blocking FIFO read is the long-running FOREGROUND tool: it parks until we
# write to the pipe (we never do) or interrupt it. We can't use `sleep` —
# Claude Code 2.1.158 refuses foreground sleeps ("Foreground sleep is blocked;
# use run_in_background"). `&&` means LATE is only written if cat completes, so
# the absence of late.txt proves the tool was killed mid-execution.
PROMPT = ("I have a named pipe at /tmp/aohook/fifo that another process will "
          "write a line to. Run exactly this with the Bash tool in the "
          "FOREGROUND (do not background it, do not modify it): "
          f"cat {FIFO} && echo LATE > {LATE}")


def main():
    aoprobe.seed_config(events=aoprobe.ALL_EVENTS, decision="allow", sleep_s=0.0)
    for p in (LATE, FIFO):
        try:
            os.remove(p)
        except OSError:
            pass
    os.mkfifo(FIFO)
    sess = aoprobe.ClaudeSession(PROMPT, BASE_URL, PTY_LOG)
    sess.start()

    bash_started = None
    esc_at = None
    while sess.elapsed() < 90:
        sess._pump_once(no_hook_yet=(not aoprobe.payloads()))
        pre_bash = [e for e in aoprobe.payloads()
                    if e["event"] == "PreToolUse" and e.get("tool") == "Bash"]
        if pre_bash and bash_started is None:
            bash_started = time.time()
        if bash_started and esc_at is None and time.time() - bash_started > 2.5:
            sess.send("\x1b")            # Esc = cancel
            esc_at = time.time()
            print(f"[probe] sent Esc at {sess.elapsed():.1f}s (tool had been running ~2.5s)")
        if esc_at and time.time() - esc_at > 8:
            break
    sess._drain(2.0)
    sess.exit()
    # Unblock any still-parked cat child (no-op if already reaped) so cleanup
    # doesn't leave a process holding the fifo. Done AFTER exit so it can't
    # race the interrupt and create a false-negative late.txt.
    try:
        fd = os.open(FIFO, os.O_WRONLY | os.O_NONBLOCK)
        os.close(fd)
    except OSError:
        pass

    rows = aoprobe.payloads()
    tpath = next((e["payload"].get("transcript_path") for e in rows
                  if e["payload"].get("transcript_path")), None)
    marker = False
    marker_text = ""
    blocked = False
    if tpath and os.path.exists(tpath):
        txt = open(tpath, errors="replace").read()
        for m in ("[Request interrupted by user for tool use]",
                  "[Request interrupted by user]", "Request interrupted by user"):
            if m in txt:
                marker = True
                i = txt.find(m)
                marker_text = txt[max(0, i - 10):i + len(m) + 10]
                break
        blocked = "tool_use_error" in txt or "Foreground `sleep` is blocked" in txt

    post_bash = [e for e in rows if e["event"] == "PostToolUse" and e.get("tool") == "Bash"]
    pre_bash = [e for e in rows if e["event"] == "PreToolUse" and e.get("tool") == "Bash"]
    fail_ev = [e for e in rows if e["event"] == "PostToolUseFailure"]
    interrupted_flags = [e["payload"].get("tool_response", {}).get("interrupted")
                         for e in post_bash]
    print("==== INTERRUPT (tool-exec) PROBE ====")
    print("timeline:", [(e.get("event"), e.get("tool")) for e in rows])
    print(f"PreToolUse Bash fired: {len(pre_bash) > 0}  (tool actually started)")
    print(f"command blocked by built-in safety (no interrupt to test): {blocked}")
    print(f"late.txt created (tool finished): {os.path.exists(LATE)}  (expect False if killed)")
    print(f"PostToolUse Bash count: {len(post_bash)}  interrupted flags: {interrupted_flags}")
    # Does an interrupt surface as a HOOK (PostToolUseFailure with is_interrupt),
    # or only as the transcript marker? (is_interrupt:false on a non-zero exit
    # implied this event also covers interrupts.)
    print(f"PostToolUseFailure fired (interrupt as a hook signal?): {len(fail_ev) > 0}")
    if fail_ev:
        fp = fail_ev[0]["payload"]
        print(f"   tool={fp.get('tool_name')}  is_interrupt={fp.get('is_interrupt')}  "
              f"error={fp.get('error')!r}")
    print(f"transcript interrupt marker present: {marker}  ctx={marker_text!r}")
    print("pty log:", PTY_LOG)
    print("=====================================")


if __name__ == "__main__":
    main()
