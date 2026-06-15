#!/usr/bin/env python3
"""Does a fresh (UNTRUSTED) worktree show a blocking dialog on claude TUI boot
that eats AO's first Send?

AO's launch.go deliberately does NOT pre-seed trust — it relies on the user's
real config, and notes a stray acceptance dialog is meant to be caught by the
stall detector → take-control (which ISN'T wired yet). A worktree switch lands
claude in a brand-new folder the user's config has never trusted. If claude's
"Do you trust the files in this folder?" dialog blocks boot, AO's gate latches on
it and writes paste+Enter into the DIALOG, not the composer → first message
swallowed; back-to-main (already trusted) works. This is the real-world condition
the trusted-worktree harness hid.

This boots cold in an UNTRUSTED worktree with AO's exact launch flags and dumps
the screen + whether the turn submits (credit-free via SlowMock). The bypass
dialog is pre-accepted so anything blocking here is the TRUST gate, isolated.

Run:  python3 probe_untrusted_worktree.py
"""
import json
import os
import re
import signal
import time

import aoprobe
import probe_worktree_boot as wb

ANSI = re.compile(rb"\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)"
                  rb"|\x1b[()][AB012]|\x1b[=>]|[\x00-\x08\x0b-\x1f]")


def screen(raw):
    return re.sub(r"\n{3,}", "\n\n", ANSI.sub(b"", raw.replace(b"\r", b"")).decode("utf-8", "replace"))


def untrust_worktree():
    """Remove the worktree from trusted projects so claude treats it as a fresh,
    never-seen folder (the real worktree-switch condition)."""
    gp = f"{aoprobe.CONFIG_DIR}/.claude.json"
    g = json.load(open(gp))
    g.get("projects", {}).pop(wb.WT, None)
    # Also drop any blanket trust that would suppress the per-folder dialog.
    g.pop("hasTrustDialogAccepted", None)
    json.dump(g, open(gp, "w"))


def boot_and_capture(prompt, mock, max_s=14):
    """Reuse the production-gate driver but also keep the full screen."""
    import fcntl, pty, select, struct, termios
    mock.reset(markers=[prompt.encode()])
    env = wb.clean_env()
    env["ANTHROPIC_BASE_URL"] = mock.url
    pid, master = pty.fork()
    if pid == 0:
        os.chdir(wb.WT)
        os.execvpe("claude", ["claude", "--permission-mode", "bypassPermissions",
                              "--allow-dangerously-skip-permissions",
                              "--thinking-display", "summarized",
                              "--model", wb.MODEL], env)
        os._exit(127)
    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
    raw, start, last = bytearray(), time.time(), time.time()
    sent_at = composer_at = None
    while time.time() - start < max_s:
        r, _, _ = select.select([master], [], [], 0.05)
        now = time.time()
        if master in r:
            try:
                d = os.read(master, 65536)
            except OSError:
                break
            if not d:
                break
            raw += d
            last = now
        norm = aoprobe._norm(bytes(raw))
        if composer_at is None and any(m in norm for m in wb.COMPOSER_MARKERS):
            composer_at = now - start
        if sent_at is None and (len(raw) >= 512 and now - last >= 0.4
                                or now - start >= 8.0):
            os.write(master, wb.cs.ao_clear()); time.sleep(wb.cs.SETTLE)
            os.write(master, wb.cs.ao_paste(prompt)); time.sleep(wb.cs.SETTLE)
            os.write(master, wb.cs.CR)
            sent_at = now - start
        if mock.submit.is_set():
            break
    time.sleep(0.4)
    for s in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.kill(pid, s)
        except OSError:
            break
        time.sleep(0.12)
    os.close(master)
    return {"submitted": mock.submit.is_set(), "sent_at": sent_at,
            "composer_at": composer_at, "screen": screen(bytes(raw))}


def main():
    if not os.path.exists(aoprobe.REAL_CREDS):
        raise SystemExit(f"no creds at {aoprobe.REAL_CREDS}")
    aoprobe.seed_config(events=[])
    wb.preaccept_and_worktree()    # sets up git worktree + skips the bypass dialog
    untrust_worktree()             # ...but make the worktree itself UNtrusted
    mock = wb.SlowMock(0.0)
    mock.start()
    print(f"UNTRUSTED worktree boot; mock {mock.url}\n")
    res = boot_and_capture("zuluuntrustmark", mock)
    print(f"submitted={res['submitted']}  gate_fired@{res['sent_at']}s  "
          f"composer@{res['composer_at']}s")
    print("\n--- screen ---")
    print(res["screen"][-1800:])
    mock.stop()
    print(f"\ntemp creds at {aoprobe.CONFIG_DIR} — delete after review.")


if __name__ == "__main__":
    main()
