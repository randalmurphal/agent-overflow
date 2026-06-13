#!/usr/bin/env python3
"""Probe: which key sequence reliably EMPTIES the claude TUI composer?

claudetui.Send pastes-and-submits assuming an empty composer. After a think-only
Esc-revert the TUI restores the just-sent prompt back INTO the composer
(LIVE-confirmed, probe_hook_escrevert.py / probe_hook_revertcontext.py), so AO's
next Send would FUSE the leftover with the new paste. This probe finds a clean,
bounded clear sequence to run before pasting.

No API turn: we paste into the composer and never submit, so no proxy and no
credit spend — only composer editing is exercised. BASE_URL is a dead address.

Detection caveat: PTY scrape is unreliable for a PARTIAL clear because a
line-scoped kill re-renders only the killed line, leaving the survivor's stale
cells untouched. So after each candidate we force a FULL repaint via a winsize
change and read the freshly painted composer. Two line-tagged markers let us
tell a FULL-buffer clear (both gone) from a LINE-only clear (one survives).
"""
import struct
import fcntl
import termios
import time

import aoprobe

BASE_URL = "http://127.0.0.1:8099"   # unused — we never submit a turn
PTY_LOG = f"{aoprobe.AOHOOK}/pty-composerclear.log"
BPS, BPE = "\x1b[200~", "\x1b[201~"   # bracketed-paste guards (same as Send)

MX = "CLEARPROBEXX"                    # first-line marker
MY = "CLEARPROBEYY"                    # second-line marker
MULTI = MX + " first line\n" + MY + " second line"

# Five line-tagged markers to pin the per-line Ctrl-U count and detect a
# partial clear precisely (which markers survive).
L5 = [f"CLR5L{i}MARK" for i in range(5)]
MULTI5 = "\n".join(f"{m} line {i}" for i, m in enumerate(L5))


def drain(sess, secs):
    end = time.time() + secs
    while time.time() < end:
        sess._pump_once(no_hook_yet=False)


def repaint(sess, cols):
    # A winsize change forces the Ink TUI to redraw EVERYTHING, so the scrape
    # reflects the composer's current content, not a stale per-line diff.
    fcntl.ioctl(sess.master, termios.TIOCSWINSZ,
                struct.pack("HHHH", sess.rows, cols, 0, 0))
    drain(sess, 1.0)


def composer_text(sess):
    sess._rawtail = b""               # observe only the post-repaint render
    repaint(sess, 160)
    repaint(sess, 200)
    return aoprobe._norm(sess._rawtail).decode("ascii", "replace")


def hard_clear(sess):
    # The proven (brute-force) reset used between candidates: Ctrl-E to end then
    # backspace everything away. Re-validated as the FULL-CLEAR control below.
    sess.send("\x05")
    for _ in range(800):
        sess.send("\x7f")
    drain(sess, 1.0)


def test(sess, name, keys):
    sess.send(BPS + MULTI + BPE)
    drain(sess, 1.5)
    before = composer_text(sess)
    seen = MX.lower() in before and MY.lower() in before
    sess.send(keys)
    drain(sess, 1.0)
    after = composer_text(sess)
    x, y = MX.lower() in after, MY.lower() in after
    if not x and not y:
        verdict = "FULL-CLEAR"
    elif x != y:
        verdict = "LINE-ONLY"
    else:
        verdict = "NO-OP"
    print(f"  {name:26} paste_seen={seen}  X_left={int(x)} Y_left={int(y)}  -> {verdict}")
    hard_clear(sess)
    return not x and not y


def test_n(sess, name, content, markers, keys):
    """Generalized: paste `content`, apply `keys`, report which markers survive."""
    sess.send(BPS + content + BPE)
    drain(sess, 1.5)
    before = composer_text(sess)
    seen = sum(m.lower() in before for m in markers)
    sess.send(keys)
    drain(sess, 1.0)
    after = composer_text(sess)
    left = [m for m in markers if m.lower() in after]
    verdict = "FULL-CLEAR" if not left else f"PARTIAL({len(left)}/{len(markers)} left)"
    print(f"  {name:28} paste_seen={seen}/{len(markers)}  -> {verdict}")
    hard_clear(sess)


def empty_safety(sess, name, keys):
    """Apply a clear to an ALREADY-empty composer, then prove it still accepts
    input (composer not stuck in a kill-ring / exit-warning state)."""
    sess.send(keys)
    drain(sess, 1.0)
    sess.send(BPS + "AFTEREMPTYCLR" + BPE)
    drain(sess, 1.2)
    accepts = "afteremptyclr" in composer_text(sess)
    print(f"  {name:28} composer still accepts input after no-op clear: {accepts}")
    hard_clear(sess)


def test_placeholder(sess, name, keys):
    """A multi-line paste collapses to a `[Pasted text #N +K lines]` chip; confirm
    the clear removes the COLLAPSED placeholder (detected via its 'pastedtext'
    text), since that's what a restored multi-line prompt becomes after a revert."""
    sess.send(BPS + MULTI5 + BPE)
    drain(sess, 1.5)
    had = "pastedtext" in composer_text(sess)
    sess.send(keys)
    drain(sess, 1.0)
    still = "pastedtext" in composer_text(sess)
    print(f"  {name:28} placeholder_before={had}  cleared={had and not still}")
    hard_clear(sess)


def main():
    aoprobe.seed_config(events=[], decision="allow")
    sess = aoprobe.ClaudeSession(None, BASE_URL, PTY_LOG, extra_args=[])
    sess.start()
    drain(sess, 7.0)                  # let the composer render (trust auto-accepts)
    print("==== COMPOSER-CLEAR (installed claude binary) ====")
    test(sess, "Ctrl-U (\\x15)", "\x15")
    test(sess, "Ctrl-U x4", "\x15\x15\x15\x15")
    test(sess, "Ctrl-E + Ctrl-U", "\x05\x15")
    test(sess, "Ctrl-A + Ctrl-K", "\x01\x0b")
    test(sess, "Ctrl-E + 800x backspace", "\x05" + "\x7f" * 800)
    print("-- per-line Ctrl-U count (5-line buffer) --")
    test_n(sess, "Ctrl-U x5 on 5 lines", MULTI5, L5, "\x15" * 5)
    test_n(sess, "Ctrl-U x7 on 5 lines", MULTI5, L5, "\x15" * 7)
    print("-- a collapsed multi-line paste placeholder is cleared by Ctrl-U --")
    test_placeholder(sess, "Ctrl-U x8 on collapsed paste", "\x15" * 8)
    print("-- excess Ctrl-U on an empty composer is a safe no-op --")
    empty_safety(sess, "Ctrl-U x16 on empty", "\x15" * 16)
    sess.exit()
    print("pty log:", PTY_LOG)
    print("==================================================")


if __name__ == "__main__":
    main()
