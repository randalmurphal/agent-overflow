#!/usr/bin/env python3
"""Tier-0 real-origin turn: claude -> Go proxy -> Bun sidecar -> REAL api.anthropic.com.

The hermetic full-topology probe proved the h1 fingerprint + casing survive the
handoff and SSE streams through all three hops on plain TCP. This probe is the
last gate: one genuine subscription turn through the same topology, against the
real origin, to exercise the two things the hermetic sink CANNOT:

  Q3 (e2e + real SNI): the turn completes and renders. The real ClientHello
     (SNI = api.anthropic.com, the version-pinned Bun's TLS) is presented to
     Anthropic's edge. We can't capture that hello without MITMing claude's own
     outbound, so a SUCCESSFUL turn IS the evidence the real-hostname fingerprint
     is accepted (per the advisor).
  content-encoding: every prior capture stripped Accept-Encoding, so whether
     Anthropic compresses an SSE response was unknown. Bun fetch now regenerates
     claude's full `Accept-Encoding: gzip, deflate, br, zstd`, so the origin MAY
     compress. The sidecar logs resp_content_encoding per relayed request; if a
     body came back compressed, the sidecar auto-decompressed it and STRIP_RESP
     dropped the stale Content-Encoding/Content-Length so claude gets coherent
     bytes — and a rendered turn proves that handling worked.

This is genuine subscription traffic: claude's real OAuth bearer, real body, real
endpoint — the whole point is that it does NOT look anomalous. Isolated
CLAUDE_CONFIG_DIR (/tmp/aoclaude, copied creds). One trivial prompt (PONG) keeps
token cost negligible. Teardown sends Esc (never Enter) so no native prompt is
accidentally accepted.
"""
import os
import json
import subprocess
import sys
import time
import uuid

import aoprobe

PROXY_BIN = os.environ.get("TIER0_PROXY", "/tmp/tier0_proxy")
BUN = os.environ.get("BUN1314", "/tmp/bun1314/bun")
SIDECAR_JS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tier0_sidecar.js")
UPSTREAM = "https://api.anthropic.com"
CAP = "/tmp/tier0-real-cap.jsonl"
SIDECAR_LOG = "/tmp/tier0-real-sidecar.log"
PTY_LOG = f"{aoprobe.AOHOOK}/pty-tier0-real.log"

SESSION_ID = str(uuid.uuid4())
TAG = SESSION_ID.split("-")[0].upper()
# Default: a one-word turn (fast, exercises encoding + render). Override via env to
# run a longer turn whose output spreads over seconds — the strong streaming proof
# (a wide chunk span can't be network jitter or buffer-then-flush).
PROMPT = os.environ.get("TIER0_REAL_PROMPT") or \
    f"Reply with exactly the single word PONG{TAG} and nothing else."
RENDER_MARK = (os.environ.get("TIER0_REAL_MARK") or f"pong{TAG}").lower().encode()


def read_jsonl(path):
    if not os.path.exists(path):
        return []
    out = []
    for line in open(path, errors="replace"):
        line = line.strip()
        if line:
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return out


def _read_port_line(proc, key, deadline_s=12):
    deadline = time.time() + deadline_s
    while time.time() < deadline:
        line = proc.stdout.readline()
        if not line:
            if proc.poll() is not None:
                return None
            continue
        try:
            return json.loads(line)[key]
        except (json.JSONDecodeError, KeyError):
            continue
    return None


def main():
    for need, what in [(BUN, "pinned Bun"), (PROXY_BIN, "tier0 proxy binary")]:
        if not os.path.exists(need):
            print(f"ABORT: {what} not found at {need}")
            sys.exit(1)
    for stale in (CAP, SIDECAR_LOG):
        try:
            os.remove(stale)
        except OSError:
            pass

    # Minimal hook set: Stop is claude's authoritative turn-complete signal.
    aoprobe.seed_config(events=["UserPromptSubmit", "Stop"], decision="allow")

    sidecar = subprocess.Popen(
        [BUN, SIDECAR_JS],
        env=dict(os.environ, SIDE_PORT="0", TIER0_SIDECAR_LOG=SIDECAR_LOG),
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    side_port = _read_port_line(sidecar, "port")
    if not side_port:
        print("ABORT: sidecar never reported a port;",
              (sidecar.stderr.read()[-400:] if sidecar.stderr else ""))
        sidecar.kill()
        sys.exit(1)

    proxy = subprocess.Popen(
        [PROXY_BIN, "--listen", "127.0.0.1:0", "--upstream", UPSTREAM,
         "--sidecar", f"http://127.0.0.1:{side_port}/", "--cap", CAP],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    listen = _read_port_line(proxy, "listen")
    if not listen:
        print("ABORT: proxy never reported listen;",
              (proxy.stderr.read()[-400:] if proxy.stderr else ""))
        sidecar.kill(); proxy.kill()
        sys.exit(1)
    base_url = f"http://{listen}"

    sess = aoprobe.ClaudeSession(
        PROMPT, base_url, PTY_LOG, extra_args=["--session-id", SESSION_ID])
    rendered = False
    try:
        sess.start()

        def until():
            nonlocal rendered
            if RENDER_MARK in aoprobe._norm(sess._rawtail):
                rendered = True
            stop = any(e.get("event") == "Stop" for e in aoprobe.payloads())
            return stop and rendered

        done = sess.run(until, max_s=120)
        sess._drain(2.0)
        keystrokes = sess.keystrokes
        rendered = rendered or (RENDER_MARK in aoprobe._norm(sess._rawtail))
    finally:
        sess.exit()
        proxy.terminate(); sidecar.terminate()
        for p in (proxy, sidecar):
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()

    # ---- analysis ----
    rows = aoprobe.payloads()
    stop_fired = any(e.get("event") == "Stop" for e in rows)

    cap = read_jsonl(CAP)
    reqs = [r for r in cap if r.get("kind") == "request"]
    heads = [r for r in cap if r.get("kind") == "response_head"]
    chunk_rows = [r for r in cap if r.get("kind") == "response_chunk"]
    errors = [r for r in cap if r.get("kind") == "error"]
    statuses = [h.get("status") for h in heads]

    # streaming span on the main (largest) response
    by_req = {}
    for r in chunk_rows:
        by_req.setdefault(r.get("req_id"), []).append(r.get("t_ms"))
    spans = {rid: round(max(ts) - min(ts), 1) for rid, ts in by_req.items() if len(ts) >= 2}
    max_span = max(spans.values()) if spans else 0.0
    streamed = max_span >= 50.0 or any(len(ts) >= 5 for ts in by_req.values())

    side_log = read_jsonl(SIDECAR_LOG)

    print("==== TIER-0 REAL TURN: claude -> Go -> Bun -> api.anthropic.com ====")
    print(f"session-id: {SESSION_ID}")
    print(f"proxy={listen}  sidecar=127.0.0.1:{side_port}  upstream={UPSTREAM}")
    print(f"keystrokes(drive)={keystrokes}  (positional prompt auto-submits; "
          f"only the trust \\r / submit-nudge are ours)\n")

    print("-- Q3: end-to-end through the real origin --")
    print(f"   requests relayed: {len(reqs)}  paths={[r.get('path') for r in reqs]}")
    print(f"   response statuses: {statuses}")
    print(f"   hook Stop fired (turn complete): {stop_fired}")
    print(f"   answer rendered in TUI ({RENDER_MARK.decode()}): {rendered}")
    if errors:
        print(f"   !! relay errors: {[(e.get('stage'), e.get('err')) for e in errors]}")

    print("\n-- streaming (hop c, real SSE) --")
    print(f"   response_chunk counts by req: "
          f"{ {rid: len(ts) for rid, ts in by_req.items()} }")
    print(f"   max response span: {max_span} ms   streamed: {streamed}")

    print("\n-- content-encoding observation (the open Tier-0 risk) --")
    if not side_log:
        print("   (sidecar log empty — no relayed request observed)")
    for i, e in enumerate(side_log):
        print(f"   relay[{i}] status={e.get('resp_status')} "
              f"content-encoding={e.get('resp_content_encoding')!r} "
              f"content-type={e.get('resp_content_type')!r}")
    encs = {e.get("resp_content_encoding") for e in side_log}
    compressed = encs - {"(none)", None}
    if compressed:
        print(f"   => origin compressed ({compressed}). Sidecar auto-decompressed +")
        print(f"      stripped stale Content-Encoding/Length; a rendered turn proves")
        print(f"      claude received coherent bytes.")
    else:
        print("   => origin returned identity (no compression) for these requests;")
        print("      content-encoding handling is moot for the SSE path.")

    ok = stop_fired and rendered and (200 in statuses) and not errors
    print("\n==== VERDICT ====")
    print(f"  Q3 e2e + real SNI accepted : {'PASS' if ok else 'FAIL'}")
    print(f"  streaming through topology : {'PASS' if streamed else 'CHECK'}")
    print(f"  content-encoding           : observed (see above)")
    if ok:
        print("  A real subscription turn renders through Go-inbound + Bun-sidecar.")
        print("  The real-hostname fingerprint is accepted; the topology is viable.")
    else:
        print("  Turn did NOT complete cleanly — inspect statuses/errors + pty log.")
    print("  cap:", CAP, "| sidecar log:", SIDECAR_LOG, "| pty:", PTY_LOG)
    print("===================================================================")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
