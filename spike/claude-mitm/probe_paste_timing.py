#!/usr/bin/env python3
"""Probe: does AO's INLINE image-send keystroke sequence land correctly on the
installed binary (2.1.170)?

AO no longer front-loads images. The composer records each image's drop point with
an "[Image #i]" marker in the message text; Send (buildSendSteps /
splitContentByImageMarkers) splits the text at those markers and pastes each image
PATH in place — text-run, image, text-run, ... — so Claude's paste handler ingests
the image where the user put it and stamps its own conversation-global "[Image #N]"
label there. This probe drives that exact interleaved sequence and verifies the
resulting transcript user message:

  1. the image lands at the INLINE position (Claude's "[Image #N]" label appears
     between the surrounding words, NOT at the front of the message), and
  2. each pasted PATH became a real image block (right count) and did NOT fuse into
     the text (the merge tell) — i.e. the 200ms pasteSettle between every two
     consecutive pastes holds across text→image and image→text boundaries, and
  3. multiple images each ingest as their own inline paste (no newline-join).

Authoritative check is the transcript (Claude's own parse of what we pasted), never
TUI scraping. Requires the loopback gateway running at AO_BASE_URL (see README) — it
forks the real `claude` and makes live /v1/messages calls.
"""
import base64
import json
import os
import re
import time

import aoprobe

BASE_URL = os.environ.get("AO_BASE_URL", "http://127.0.0.1:8091")
IMG_DIR = f"{aoprobe.AOHOOK}/paste-timing"

# Mirror internal/provider/claudetui/session_send.go exactly.
BPM_START, BPM_END = "\x1b[200~", "\x1b[201~"
CLEAR = "\x15" * 16            # composerClearKeystrokes Ctrl-U
COMPOSER_SETTLE = 0.060        # composerSettle (clear→firstpaste, lastpaste→submit)
PASTE_SETTLE = 0.200          # pasteSettle (claudePasteCompletionWindow + 100ms)

# A valid 1x1 PNG (content irrelevant — we test INGESTION/framing/position, not the
# model's description). Distinct PATHS are what matter for the multi-image split.
PNG_1X1 = base64.b64decode(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")


def bracketed(s: str) -> str:
    return BPM_START + s.replace(BPM_END, "") + BPM_END


def latest_user_message(marker: str):
    """Content list of the latest transcript user message whose text contains
    `marker`, else None. Scans every hook-reported transcript."""
    found = None
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
            if not isinstance(msg, dict) or msg.get("role") != "user":
                continue
            content = msg.get("content")
            if not isinstance(content, list):
                continue
            text = " ".join(b.get("text", "") for b in content
                            if isinstance(b, dict) and b.get("type") == "text")
            if marker in text:
                found = content
    return found


def drive_pastes(sess, pastes):
    """Replay buildSendSteps for an ordered list of bracketed-paste payloads
    (interleaved text runs and image paths, empty runs already dropped): clear ->
    [paste, ...] -> submit, with pasteSettle between consecutive pastes and
    composerSettle after the clear and before the submit."""
    sess.send(CLEAR)
    time.sleep(COMPOSER_SETTLE)
    for i, payload in enumerate(pastes):
        sess.send(bracketed(payload))
        time.sleep(PASTE_SETTLE if i < len(pastes) - 1 else COMPOSER_SETTLE)
    sess.send("\r")


def run_scenario(name, pastes, marker, image_paths):
    """Fresh session, drive one inline send, return the parsed transcript facts."""
    sess = aoprobe.ClaudeSession(prompt=None, base_url=BASE_URL,
                                 pty_log=f"{IMG_DIR}/pty-{name}.log")
    sess.start()
    t0 = time.time()
    while time.time() - t0 < 12 and not any(
            e["event"] == "SessionStart" for e in aoprobe.payloads()):
        sess._pump_once(no_hook_yet=False)
    time.sleep(1.5)
    sess._drain(0.5)

    drive_pastes(sess, pastes)

    content = None
    t0 = time.time()
    while time.time() - t0 < 90:
        sess._pump_once(no_hook_yet=False)
        content = latest_user_message(marker)
        if content is not None:
            break
    sess._drain(2.0)
    sess.exit()

    if content is None:
        return None
    text = " ".join(b.get("text", "") for b in content
                    if isinstance(b, dict) and b.get("type") == "text")
    num_images = sum(1 for b in content
                     if isinstance(b, dict) and b.get("type") == "image")
    fused = any(p in text for p in image_paths)
    return {"images": num_images, "text": text, "fused": fused,
            "raw_types": [b.get("type") for b in content if isinstance(b, dict)]}


def main():
    aoprobe.seed_config(events=["SessionStart", "UserPromptSubmit", "Stop"])
    os.makedirs(IMG_DIR, exist_ok=True)
    img1 = f"{IMG_DIR}/one.png"
    img2 = f"{IMG_DIR}/two.png"
    for p in (img1, img2):
        with open(p, "wb") as f:
            f.write(PNG_1X1)

    # Each scenario: (name, ordered paste payloads, unique text marker to locate the
    # message, image paths used, expected image-block count, regex the transcript
    # text must match — proving Claude's "[Image #N]" label sits INLINE where we
    # pasted the path, not at the front).
    scenarios = [
        ("inline_middle",
         ["describe ZQXmid the object in ", img1, " only please"],
         "ZQXmid", [img1], 1,
         re.compile(r"the object in \[Image #\d+\] only please")),
        ("two_inline",
         ["ZQXtwo first ", img1, " and second ", img2, " end"],
         "ZQXtwo", [img1, img2], 2,
         re.compile(r"first \[Image #\d+\] and second \[Image #\d+\] end")),
    ]

    print("==== INLINE PASTE-PLACEMENT PROBE (2.1.170) ====")
    all_ok = True
    for name, pastes, marker, paths, want_images, want_re in scenarios:
        res = run_scenario(name, pastes, marker, paths)
        if res is None:
            print(f"[{name}] NO user message reached the transcript — inconclusive")
            all_ok = False
            continue
        inline_ok = bool(want_re.search(res["text"]))
        front_loaded = res["text"].lstrip().startswith("[Image #")
        ok = (res["images"] == want_images and inline_ok
              and not res["fused"] and not front_loaded)
        all_ok = all_ok and ok
        print(f"[{name}] image_blocks={res['images']} (want {want_images}) "
              f"inline_position={inline_ok} front_loaded={front_loaded} "
              f"fused_with_path={res['fused']} blocks={res['raw_types']}\n"
              f"           text={res['text']!r} -> {'PASS' if ok else 'FAIL'}")
    verdict = ("CONFIRMED: AO's inline sequence places each image at its marker "
               "(Claude labels it in position) and the 200ms gap holds across "
               "text/image boundaries on 2.1.170"
               if all_ok else
               "NOT CONFIRMED — inspect transcripts/pty logs; check the inline "
               "split or the paste gaps")
    print(f"VERDICT: {verdict}")
    print(f"pty logs: {IMG_DIR}/")
    print("================================================")


if __name__ == "__main__":
    main()
