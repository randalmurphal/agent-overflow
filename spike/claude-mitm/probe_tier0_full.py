#!/usr/bin/env python3
"""Tier-0 full-topology hermetic test: Go-inbound + Bun-sidecar + sink.

probe_h1_headerforms.py already proved that Bun fetch, handed claude's 17 app
headers as a plain object, emits claude's COMPLETE wire block (CLAUDE_FULL =
app sorted + regenerated framing). probe_tier0_stream.py proved both Bun streaming
hops (a,b). This probe wires the real topology and proves those results survive
the actual handoff path AO would use:

   synthetic claude request
      -> Go proxy (RAW read: preserves claude's header casing)   [§12 rule 2]
      -> JSON envelope (framing stripped)                        [§12 rule 3]
      -> Bun sidecar (rebuild plain object, version-pinned fetch)[§12 rule 1]
      -> hermetic sink (captures fetch's wire block + streams SSE)

Assertions:
  Q2(i)  h1 block: the sink sees CLAUDE_FULL — claude's exact 17 app headers in
         case-sensitive order, then fetch's regenerated framing.
  Q2(ii) casing survived the handoff: the sidecar's handed-to-fetch NAMES (the
         INPUT side) == CLAUDE_APP; the sink capture (the OUTPUT side) == CLAUDE_FULL.
         Both ends carry claude's Title-Case — the Go raw-read -> envelope ->
         object-rebuild chain never lowercased.
  Q1(c)  hop (c): the Go flush-relay streams — response_chunk arrival times in the
         proxy capture span ~= (EVENTS-1)*DELAY (not all-at-once at close).
  body   integrity: the synthetic client receives all EVENTS SSE events intact
         through the de-chunk/re-chunk relay.

Hermetic: sink is plain TCP on 127.0.0.1; the TLS ClientHello is NOT exercised
here (invariant under forwarding, already proven == claude by ja3_diff.py). No
real Anthropic traffic, no tokens. Authorization is a synthetic bearer.
"""
import json
import os
import socket
import subprocess
import sys
import time

import tier0_sink
from probe_h1_headerforms import CLAUDE_APP, CLAUDE_FRAMING, CLAUDE_FULL

BUN = os.environ.get("BUN1314", "/tmp/bun1314/bun")
PROXY_BIN = os.environ.get("TIER0_PROXY", "/tmp/tier0_proxy")
SIDECAR_JS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "tier0_sidecar.js")
PROXY_CAP = "/tmp/tier0-full-cap.jsonl"
SIDECAR_LOG = "/tmp/tier0-full-sidecar.log"

EVENTS = 8
DELAY = 0.10
SPAN_FLOOR_MS = (EVENTS - 1) * DELAY * 1000 * 0.5     # >=350 ms => streamed

# synthetic values for claude's 17 app header names (no real credentials)
SYN_VALUES = {
    "Accept": "application/json",
    "Authorization": "Bearer synthetic-not-a-real-token",
    "Content-Type": "application/json",
    "User-Agent": "claude-cli/synthetic (external)",
    "X-Claude-Code-Session-Id": "00000000-0000-0000-0000-000000000000",
    "X-Stainless-Arch": "x64",
    "X-Stainless-Lang": "js",
    "X-Stainless-OS": "Linux",
    "X-Stainless-Package-Version": "0.0.0",
    "X-Stainless-Retry-Count": "0",
    "X-Stainless-Runtime": "bun",
    "X-Stainless-Runtime-Version": "1.3.14",
    "X-Stainless-Timeout": "600",
    "anthropic-beta": "synthetic-beta",
    "anthropic-dangerous-direct-browser-access": "true",
    "anthropic-version": "2023-06-01",
    "x-app": "cli",
}


def read_jsonl(path):
    if not os.path.exists(path):
        return []
    out = []
    for line in open(path):
        line = line.strip()
        if line:
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                pass
    return out


def synth_request(proxy_host, proxy_port):
    """A faithful claude request: app headers (claude order) then claude's own
    framing, which the proxy strips and fetch regenerates."""
    body = json.dumps({"model": "synthetic", "messages": [], "stream": True})
    lines = ["POST /v1/messages?beta=true HTTP/1.1"]
    for name in CLAUDE_APP:
        lines.append(f"{name}: {SYN_VALUES[name]}")
    lines.append("Connection: keep-alive")
    lines.append(f"Host: {proxy_host}:{proxy_port}")
    lines.append("Accept-Encoding: gzip, deflate, br, zstd")
    lines.append(f"Content-Length: {len(body)}")
    return ("\r\n".join(lines) + "\r\n\r\n" + body).encode()


def dechunk(body):
    out, i = b"", 0
    while i < len(body):
        j = body.find(b"\r\n", i)
        if j < 0:
            break
        try:
            size = int(body[i:j], 16)
        except ValueError:
            break
        if size == 0:
            break
        out += body[j + 2: j + 2 + size]
        i = j + 2 + size + 2
    return out


def read_response(sock):
    sock.settimeout(20)
    t0 = time.monotonic()
    arrivals, data = [], b""
    while True:
        try:
            chunk = sock.recv(65536)
        except socket.timeout:
            break
        if not chunk:
            break
        arrivals.append(round((time.monotonic() - t0) * 1000, 1))
        data += chunk
    span = round(arrivals[-1] - arrivals[0], 1) if len(arrivals) >= 2 else 0.0
    return data, arrivals, span


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
    for stale in (PROXY_CAP, SIDECAR_LOG):
        try:
            os.remove(stale)
        except OSError:
            pass

    sink = tier0_sink.start_sink(events=EVENTS, delay=DELAY)
    sink_url_base = f"http://127.0.0.1:{sink.port}"

    sidecar = subprocess.Popen(
        [BUN, SIDECAR_JS],
        env=dict(os.environ, SIDE_PORT="0", TIER0_SIDECAR_LOG=SIDECAR_LOG),
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    side_port = _read_port_line(sidecar, "port")
    if not side_port:
        print("ABORT: sidecar never reported a port;",
              (sidecar.stderr.read()[-400:] if sidecar.stderr else ""))
        sink.stop(); sidecar.kill()
        sys.exit(1)

    proxy = subprocess.Popen(
        [PROXY_BIN, "--listen", "127.0.0.1:0",
         "--upstream", sink_url_base,
         "--sidecar", f"http://127.0.0.1:{side_port}/",
         "--cap", PROXY_CAP],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    listen = _read_port_line(proxy, "listen")
    if not listen:
        print("ABORT: proxy never reported listen;",
              (proxy.stderr.read()[-400:] if proxy.stderr else ""))
        sink.stop(); sidecar.kill(); proxy.kill()
        sys.exit(1)
    phost, pport = listen.rsplit(":", 1)

    try:
        s = socket.create_connection((phost, int(pport)), timeout=10)
        s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        s.sendall(synth_request(phost, int(pport)))
        data, arrivals, client_span = read_response(s)
        s.close()
        # let the sink's capture + sidecar log settle
        for _ in range(20):
            if sink.captured:
                break
            time.sleep(0.1)
    finally:
        proxy.terminate()
        sidecar.terminate()
        for p in (proxy, sidecar):
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill()
        sink.stop()

    # ---- analysis ----
    head, _, body = data.partition(b"\r\n\r\n")
    sse = dechunk(body)
    n_events = sse.count(b"event: chunk")

    sink_names = sink.captured[0]["header_names"] if sink.captured else []
    side_log = read_jsonl(SIDECAR_LOG)
    handed = side_log[-1].get("handed_to_fetch", []) if side_log else []

    cap = read_jsonl(PROXY_CAP)
    chunk_rows = [r for r in cap if r.get("kind") == "response_chunk"]
    chunk_ts = [r["t_ms"] for r in chunk_rows]
    proxy_span = round(max(chunk_ts) - min(chunk_ts), 1) if len(chunk_ts) >= 2 else 0.0

    app_at_sink = [n for n in sink_names if n in CLAUDE_APP]
    framing_at_sink = [n for n in sink_names if n in CLAUDE_FRAMING]

    q2i = sink_names == CLAUDE_FULL
    q2ii = handed == CLAUDE_APP and app_at_sink == CLAUDE_APP
    q1c = proxy_span >= SPAN_FLOOR_MS
    body_ok = n_events == EVENTS

    print("==== TIER-0 FULL TOPOLOGY (hermetic): Go-inbound + Bun-sidecar + sink ====")
    print(f"proxy={listen}  sidecar=127.0.0.1:{side_port}  sink=127.0.0.1:{sink.port}")
    print(f"client recv span={client_span} ms over {len(arrivals)} reads\n")

    print("-- Q2(i) h1 block at sink (fetch's wire output) --")
    print(f"   match CLAUDE_FULL: {q2i}")
    if not q2i:
        print(f"     want: {CLAUDE_FULL}")
        print(f"     got : {sink_names}")
    else:
        print(f"     app: {app_at_sink}")
        print(f"     framing: {framing_at_sink}")

    print("\n-- Q2(ii) casing survived the Go->envelope->fetch handoff --")
    print(f"   sidecar handed-to-fetch (INPUT)  == CLAUDE_APP: {handed == CLAUDE_APP}")
    print(f"   sink app block       (OUTPUT)    == CLAUDE_APP: {app_at_sink == CLAUDE_APP}")
    if handed != CLAUDE_APP:
        print(f"     handed: {handed}")

    print("\n-- Q1(c) Go flush-relay streams (hop c) --")
    print(f"   proxy response_chunk count={len(chunk_rows)}  span={proxy_span} ms "
          f"(floor {SPAN_FLOOR_MS:.0f} ms): {q1c}")

    print("\n-- body integrity through de-chunk/re-chunk --")
    print(f"   SSE events received: {n_events}/{EVENTS}: {body_ok}")

    ok = q2i and q2ii and q1c and body_ok
    print("\n==== VERDICT ====")
    print(f"  Q2(i) h1 block        : {'PASS' if q2i else 'FAIL'}")
    print(f"  Q2(ii) casing survives: {'PASS' if q2ii else 'FAIL'}")
    print(f"  Q1(c) hop-c streaming : {'PASS' if q1c else 'FAIL'}")
    print(f"  body integrity        : {'PASS' if body_ok else 'FAIL'}")
    if ok:
        print("  Full topology preserves claude's h1 fingerprint AND streams SSE")
        print("  end-to-end. Only the real-origin turn (SNI + content-encoding) remains.")
    print("  cap:", PROXY_CAP, "| sidecar log:", SIDECAR_LOG)
    print("=========================================================================")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
