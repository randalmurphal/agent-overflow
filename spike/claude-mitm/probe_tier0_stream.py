#!/usr/bin/env python3
"""Tier-0 Stage (a)+(b): isolate the two least-proven SSE streaming hops.

Q1 ("does the chain stream SSE incrementally?") is really THREE hops:
  (a) Bun `fetch` READS the upstream SSE body incrementally    <- this probe
  (b) Bun.serve WRITES a Response(stream) body incrementally   <- this probe
  (c) Go reads Bun.serve incrementally                         <- already proven by
      proxy/main.go's flush-per-chunk relay; re-checked end-to-end in the
      full-topology probe.

Per the advisor, hop (b) — Bun.serve streaming a ReadableStream Response without
buffering-until-close — is the least-proven, so isolate it from (a) and from the
Go/envelope machinery. If we built the full topology and chunks arrived batched,
we couldn't tell (a) from (b) from the envelope. Here each stage points straight
at the hermetic sink, which flushes EVENTS SSE chunks DELAY apart.

Discriminator = the ARRIVAL SPAN at the reader (last arrival - first arrival):
  streamed  => span ~= (EVENTS-1)*DELAY   (chunks spread out as the sink emits)
  buffered  => span ~= 0                  (everything lands at once, on close)
Each stage measures span inside ONE clock domain (Bun's perf clock for (a),
Python's monotonic for (b)), so no cross-process clock sync is needed.

Hermetic: the sink is plain TCP on 127.0.0.1, no real Anthropic traffic, no
tokens, no credentials.
"""
import json
import os
import socket
import subprocess
import sys
import tempfile
import time

import tier0_sink

BUN = os.environ.get("BUN1314", "/tmp/bun1314/bun")
EVENTS = 8
DELAY = 0.10                                   # seconds between sink chunks
SPAN_EXPECTED_MS = (EVENTS - 1) * DELAY * 1000  # 700 ms if perfectly streamed
SPAN_STREAM_FLOOR_MS = SPAN_EXPECTED_MS * 0.5   # >=350 ms => clearly spread out
SPAN_BUFFER_CEIL_MS = 60.0                      # <=60 ms => clearly batched

# Stage (a): a bare Bun fetch that times each reader.read() arrival in Bun's own
# perf clock. No Go, no Bun.serve, no envelope — isolates fetch's read-streaming.
FETCH_SCRIPT = r"""
(async () => {
  const url = process.env.SINK_URL;
  const t0 = performance.now();
  let res;
  try {
    res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ stage: "a" }),
    });
  } catch (e) {
    console.log(JSON.stringify({ error: String(e) }));
    return;
  }
  const reader = res.body.getReader();
  const arrivals = [];
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    arrivals.push(Math.round((performance.now() - t0) * 10) / 10);
  }
  const span = arrivals.length >= 2 ? arrivals[arrivals.length - 1] - arrivals[0] : 0;
  console.log(JSON.stringify({
    status: res.status,
    n_reads: arrivals.length,
    arrivals,
    span_ms: Math.round(span * 10) / 10,
  }));
})();
"""

# Stage (b): a MINIMAL Bun.serve that fetches the sink and returns
# Response(up.body). Deliberately no envelope, no header rebuild — the ONLY thing
# under test is whether Bun.serve flushes a ReadableStream Response incrementally.
SERVE_SCRIPT = r"""
const SINK_URL = process.env.SINK_URL;
const server = Bun.serve({
  port: 0,
  hostname: "127.0.0.1",
  async fetch(req) {
    const up = await fetch(SINK_URL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ stage: "b" }),
    });
    return new Response(up.body, {
      status: up.status,
      headers: { "Content-Type": "text/event-stream" },
    });
  },
});
console.log(JSON.stringify({ port: server.port }));
"""


def _write_tmp(suffix, content):
    fd, path = tempfile.mkstemp(suffix=suffix, prefix="tier0_")
    with os.fdopen(fd, "w") as f:
        f.write(content)
    return path


def stage_a(sink_url):
    """Bare bun fetch; returns the parsed JSON the Bun process prints."""
    path = _write_tmp(".js", FETCH_SCRIPT)
    try:
        env = dict(os.environ, SINK_URL=sink_url)
        proc = subprocess.run([BUN, path], env=env, capture_output=True,
                              text=True, timeout=25)
    finally:
        os.unlink(path)
    out = proc.stdout.strip().splitlines()
    for line in reversed(out):
        try:
            return json.loads(line)
        except json.JSONDecodeError:
            continue
    return {"error": f"no JSON on stdout; rc={proc.returncode} "
                     f"stderr={proc.stderr[-400:]!r}"}


def measure_client_spans(host, port):
    """Connect raw, GET, timestamp every recv in Python's monotonic clock."""
    s = socket.create_connection((host, port), timeout=15)
    s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    s.sendall(b"GET / HTTP/1.1\r\nHost: sidecar\r\nConnection: close\r\n"
              b"Accept: text/event-stream\r\n\r\n")
    s.settimeout(15)
    arrivals = []
    t0 = time.monotonic()
    while True:
        try:
            chunk = s.recv(65536)
        except socket.timeout:
            break
        if not chunk:
            break
        arrivals.append(round((time.monotonic() - t0) * 1000, 1))
    s.close()
    span = round(arrivals[-1] - arrivals[0], 1) if len(arrivals) >= 2 else 0.0
    return arrivals, span


def stage_b(sink_url):
    """Minimal Bun.serve in front of the sink; measure span at a raw client."""
    path = _write_tmp(".js", SERVE_SCRIPT)
    env = dict(os.environ, SINK_URL=sink_url)
    proc = subprocess.Popen([BUN, path], env=env, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, text=True)
    try:
        port = None
        deadline = time.time() + 12
        while time.time() < deadline:
            line = proc.stdout.readline()
            if not line:
                if proc.poll() is not None:
                    break
                continue
            try:
                port = json.loads(line).get("port")
                break
            except json.JSONDecodeError:
                continue
        if not port:
            err = proc.stderr.read()[-400:] if proc.stderr else ""
            return {"error": f"Bun.serve never reported a port; stderr={err!r}"}
        arrivals, span = measure_client_spans("127.0.0.1", int(port))
        return {"port": port, "n_reads": len(arrivals), "arrivals": arrivals,
                "span_ms": span}
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


def verdict(span_ms):
    if span_ms >= SPAN_STREAM_FLOOR_MS:
        return "STREAMS (incremental)"
    if span_ms <= SPAN_BUFFER_CEIL_MS:
        return "BUFFERS (batched at close)"
    return "AMBIGUOUS (between floor and ceiling — inspect arrivals)"


def main():
    if not os.path.exists(BUN):
        print(f"ABORT: pinned Bun not found at {BUN} (set BUN1314).")
        sys.exit(1)

    sink = tier0_sink.start_sink(events=EVENTS, delay=DELAY)
    sink_url = f"http://127.0.0.1:{sink.port}/v1/messages?beta=true"
    print("==== TIER-0 STAGE (a)+(b): isolate Bun SSE streaming hops ====")
    print(f"bun: {BUN}")
    print(f"sink: {sink_url}  ({EVENTS} events, {DELAY*1000:.0f} ms apart)")
    print(f"expected streamed span ~= {SPAN_EXPECTED_MS:.0f} ms | "
          f"stream floor {SPAN_STREAM_FLOOR_MS:.0f} ms | "
          f"buffer ceil {SPAN_BUFFER_CEIL_MS:.0f} ms\n")

    a = stage_a(sink_url)
    print("-- Stage (a): bare `bun fetch` reads upstream SSE --")
    if "error" in a:
        print(f"   ERROR: {a['error']}")
        a_ok = False
    else:
        print(f"   status={a.get('status')} reads={a.get('n_reads')} "
              f"span={a.get('span_ms')} ms  arrivals={a.get('arrivals')}")
        print(f"   => {verdict(a.get('span_ms', 0))}")
        a_ok = a.get("span_ms", 0) >= SPAN_STREAM_FLOOR_MS

    b = stage_b(sink_url)
    print("\n-- Stage (b): minimal Bun.serve streams Response(up.body) --")
    if "error" in b:
        print(f"   ERROR: {b['error']}")
        b_ok = False
    else:
        print(f"   port={b.get('port')} reads={b.get('n_reads')} "
              f"span={b.get('span_ms')} ms  arrivals={b.get('arrivals')}")
        print(f"   => {verdict(b.get('span_ms', 0))}")
        b_ok = b.get("span_ms", 0) >= SPAN_STREAM_FLOOR_MS

    sink.stop()

    print("\n==== VERDICT ====")
    print(f"  hop (a) bun fetch read-streaming:  {'PASS' if a_ok else 'FAIL'}")
    print(f"  hop (b) Bun.serve write-streaming: {'PASS' if b_ok else 'FAIL'}")
    if a_ok and b_ok:
        print("  Both isolated hops stream incrementally. Safe to wire Go+envelope.")
    elif a_ok and not b_ok:
        print("  (b) BUFFERS — Bun.serve does not stream Response(stream) here.")
        print("  Fallback per advisor: Bun writes chunks to stdout, Go reads stdout.")
    else:
        print("  Re-inspect: (a) failing means fetch itself isn't read-streaming.")
    print("=================================================================")


if __name__ == "__main__":
    main()
