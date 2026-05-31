#!/usr/bin/env python3
"""Probe: can a PTY driver inject an IMAGE into interactive Claude — i.e. recover
AO's base64 image-block input? Strategy A from the coverage map: write the image
to a temp file, then bracketed-paste its PATH into the composer; Claude's paste
handler (tryReadImageFromPath) reads the bytes into a real `image` content block.

We launch with an EMPTY composer (prompt=None), paste the path, type a question,
submit, and then verify an `image` content block actually reached the model —
checked in the transcript user message (authoritative), corroborated by the
[Image #N] placeholder. Never scrapes TUI state for the result.
"""
import base64
import json
import os
import time

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
IMG = "/tmp/aohook/test.png"
PTY_LOG = f"{aoprobe.AOHOOK}/pty-attach.log"
# A valid 1x1 PNG. Content is irrelevant — we're testing INGESTION as an image
# block, not the model's description.
PNG_1X1 = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")


def has_image_block():
    """True once a user-message image content block lands in the transcript."""
    for e in aoprobe.payloads():
        tpath = e["payload"].get("transcript_path")
        if not tpath or not os.path.exists(tpath):
            continue
        for ln in open(tpath, errors="replace"):
            try:
                m = json.loads(ln)
            except (json.JSONDecodeError, ValueError):
                continue
            msg = m.get("message", {})
            content = msg.get("content") if isinstance(msg, dict) else None
            if isinstance(content, list):
                for b in content:
                    if isinstance(b, dict) and b.get("type") == "image":
                        return True, b.get("source", {}).get("type")
        return False, None
    return False, None


def main():
    aoprobe.seed_config(events=["SessionStart", "UserPromptSubmit", "PreToolUse", "Stop"])
    with open(IMG, "wb") as f:
        f.write(PNG_1X1)

    sess = aoprobe.ClaudeSession(prompt=None, base_url=BASE_URL, pty_log=PTY_LOG)
    sess.start()
    # Wait for the composer to be ready (SessionStart hook fired + a beat).
    t0 = time.time()
    while time.time() - t0 < 12 and not any(
            e["event"] == "SessionStart" for e in aoprobe.payloads()):
        sess._pump_once(no_hook_yet=False)
    time.sleep(1.5)
    sess._drain(0.5)

    # Strategy A: bracketed-paste the image PATH, then a prompt, then submit.
    sess.send("\x1b[200~" + IMG + "\x1b[201~")
    time.sleep(1.5)
    sess._drain(1.0)
    sess.send("What color is this image? Answer in one word.")
    time.sleep(0.8)
    sess.send("\r")

    ok = False
    src_type = None
    t0 = time.time()
    while time.time() - t0 < 90:
        sess._pump_once(no_hook_yet=False)
        ok, src_type = has_image_block()
        if ok:
            break
    sess._drain(3.0)
    keystrokes_total = sess.keystrokes
    sess.exit()

    # PTY placeholder corroboration (forensic only).
    placeholder = False
    try:
        import re
        d = open(PTY_LOG, "rb").read()
        t = re.sub(rb"\x1b\[[0-9;?]*[a-zA-Z]", b"", d).decode("utf-8", "replace")
        placeholder = bool(re.search(r"\[Image #\d+\]", t)) or "Image #" in t
    except OSError:
        pass

    rows = aoprobe.payloads()
    print("==== ATTACHMENT (image paste) PROBE ====")
    print("timeline:", [(e.get("event"), e.get("tool")) for e in rows])
    print(f"image content block reached model (transcript): {ok}  source.type={src_type}")
    print(f"[Image #N] placeholder seen in PTY (corroboration): {placeholder}")
    print(f"total keystrokes (path paste + prompt + enter): {keystrokes_total}")
    verdict = ("CONFIRMED: temp-file + bracketed-paste yields a real image block"
               if ok else "NOT CONFIRMED — inspect transcript / pty log")
    print(f"VERDICT: {verdict}")
    print("pty log:", PTY_LOG)
    print("========================================")


if __name__ == "__main__":
    main()
