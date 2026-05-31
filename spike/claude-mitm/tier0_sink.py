#!/usr/bin/env python3
"""Tier-0 hermetic origin sink — stands in for api.anthropic.com.

Lets the streaming + fingerprint legs be measured with NO real traffic and NO
tokens. Two jobs:

  1. CAPTURE the inbound HTTP/1.1 request's raw header block (names with original
     casing, in wire order) via probe_h1_serialize._parse. A test asserts the Bun
     sidecar emitted claude's exact h1 block after the Go->Bun handoff (Q2: the
     h1 layer + casing survival). The TLS ClientHello is deliberately NOT measured
     here — it is invariant under forwarding and already proven == claude by
     ja3_diff.py; this sink is plain TCP on 127.0.0.1 by design.
  2. RESPOND with a chunked SSE stream of N events spaced DELAY apart, each chunk
     flushed immediately (TCP_NODELAY), so a downstream reader can tell incremental
     streaming (Q1) from buffer-until-close. Each event carries its server emit
     time so a reader can compute per-hop arrival latency.

Importable (`start_sink`) for probes, or runnable for manual `curl -N` checks.
Hermetic mode never sees real credentials; it still refuses to log Authorization
values defensively (a sidecar/proxy bug could point a real request at it).
"""
import json
import os
import socket
import threading
import time

import probe_h1_serialize as h1

# Monotonic clock shared across the process so emit/arrival times are comparable.
_T0 = time.monotonic()


def now_ms():
    return round((time.monotonic() - _T0) * 1000, 1)


def _redact(headers):
    """headers: list of (name, value). Redact credential values for logging."""
    out = []
    for name, value in headers:
        low = name.lower()
        if low in ("authorization", "cookie", "x-api-key") or low.endswith("-token"):
            value = f"<redacted len={len(value)}>"
        out.append((name, value))
    return out


class Sink:
    def __init__(self, events=8, delay=0.05, status=200):
        self.events = events
        self.delay = delay
        self.status = status
        self.captured = []          # one dict per received request
        self._srv = None
        self._thread = None
        self._stop = threading.Event()
        self.port = None

    def start(self):
        self._srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._srv.bind(("127.0.0.1", 0))
        self.port = self._srv.getsockname()[1]
        self._srv.listen(16)
        self._srv.settimeout(0.3)
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()
        return self

    def _serve(self):
        while not self._stop.is_set():
            try:
                conn, _ = self._srv.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn):
        conn.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        conn.settimeout(10)
        try:
            buf = h1._read_until(conn, b"\r\n\r\n")
            head, _, rest = buf.partition(b"\r\n\r\n")
            rec = h1._parse(head + b"\r\n\r\n")
            # drain a declared body (claude POSTs JSON); we don't need its content
            clen = 0
            for name, value in rec["headers"]:
                if name.lower() == "content-length":
                    clen = int(value or "0")
            body = rest
            while len(body) < clen:
                more = conn.recv(65536)
                if not more:
                    break
                body += more
            self.captured.append({
                "recv_ms": now_ms(),
                "method": rec["method"],
                "path": rec["path"],
                "headers": _redact(rec["headers"]),       # (name, value) wire order
                "header_names": [n for n, _ in rec["headers"]],
                "body_len": len(body),
            })
            self._stream_sse(conn)
        except (OSError, ValueError, KeyError):
            pass
        finally:
            try:
                conn.close()
            except OSError:
                pass

    def _stream_sse(self, conn):
        # Chunked SSE; each event is its own chunk, flushed then DELAY slept, so a
        # reader observing incremental arrival proves the whole chain streams.
        reason = "OK" if self.status == 200 else "ERR"
        head = (
            f"HTTP/1.1 {self.status} {reason}\r\n"
            "Content-Type: text/event-stream\r\n"
            "Transfer-Encoding: chunked\r\n"
            "Connection: close\r\n\r\n"
        ).encode()
        conn.sendall(head)
        for i in range(self.events):
            payload = (
                "event: chunk\n"
                f"data: {json.dumps({'i': i, 'emit_ms': now_ms()})}\n\n"
            ).encode()
            chunk = f"{len(payload):x}\r\n".encode() + payload + b"\r\n"
            conn.sendall(chunk)
            if i < self.events - 1:
                time.sleep(self.delay)
        conn.sendall(b"0\r\n\r\n")       # terminating chunk

    def stop(self):
        self._stop.set()
        try:
            self._srv.close()
        except OSError:
            pass


def start_sink(events=8, delay=0.05, status=200):
    return Sink(events=events, delay=delay, status=status).start()


if __name__ == "__main__":
    import sys
    ev = int(os.environ.get("SINK_EVENTS", "8"))
    dl = float(os.environ.get("SINK_DELAY", "0.10"))
    s = start_sink(events=ev, delay=dl)
    print(f"sink on http://127.0.0.1:{s.port}  ({ev} events, {dl}s apart)")
    print(f"  manual: curl -N -s http://127.0.0.1:{s.port}/v1/messages")
    try:
        while True:
            time.sleep(1)
            if s.captured:
                print(f"  captured {len(s.captured)} request(s); last names="
                      f"{s.captured[-1]['header_names']}")
                s.captured.clear()
    except KeyboardInterrupt:
        s.stop()
        sys.exit(0)
