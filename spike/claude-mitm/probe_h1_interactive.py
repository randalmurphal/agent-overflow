#!/usr/bin/env python3
"""Spike: does INTERACTIVE claude emit the same HTTP/1.1 request header block as
headless `claude -p`?

All the h1 serialization work (probe_h1_serialize, probe_h1_headerforms) captured
claude's native headers via `claude -p` (headless). But AO drives claude in its
INTERACTIVE TUI. The serializer is the same Bun + Anthropic-SDK fetch path in both
modes, so the order/casing is expected to match — but this session's lesson is
MEASURE, don't infer (inferring "fetch reproduces claude by construction" was wrong
until measured). So we measure both in one run and diff.

Why it matters for the architecture even though the proxy forwards claude's OWN
headers verbatim: it confirms there's no interactive-only header the integration
must anticipate, and that the case-sensitive-ASCII-sorted block proven against
headless is exactly what the live (interactive) proxy will see and reproduce.

Method:
  - headless leg:   reuse probe_h1_serialize.capture(spawn_claude_to) — claude -p
    pointed straight at a raw http sink.
  - interactive leg: PTY-spawn interactive claude (aoprobe.ClaudeSession) pointed
    at a concurrent raw http sink; submit one prompt; capture POST /v1/messages.

Both legs hit a plaintext http:// sink (ANTHROPIC_BASE_URL), so no real Anthropic
call happens — claude's request is captured locally and gets a stub response. We
diff e2e (application) header name + order + casing.

SECURITY: interactive uses the isolated CLAUDE_CONFIG_DIR (/tmp/aoclaude) with
COPIED creds (aoprobe.seed_config), so the POST carries a real OAuth bearer; header
VALUES are redacted to `<redacted len=N>` by probe_h1_serialize._parse before
anything is stored/printed. This probe studies only names/order/casing.
"""
import socket
import threading
import time

import aoprobe
import probe_h1_serialize as h1

PTY_LOG = "/tmp/h1_interactive_pty.log"


def headless_target():
    """claude -p straight at a raw sink; return its POST /v1/messages record."""
    recs = h1.capture(h1.spawn_claude_to)
    return next((r for r in recs if h1._is_target(r)), None)


def interactive_target(max_s=90):
    """Interactive claude in a PTY, pointed at a concurrent raw sink; return its
    POST /v1/messages record (or None)."""
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", 0))
    port = srv.getsockname()[1]
    srv.listen(16)

    result = {"target": None}
    stop = threading.Event()
    deadline = time.time() + max_s
    # SSE-ish stub: keep-alive so claude sends its preflight then the real POST.
    resp = (b"HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\n"
            b"content-length: 0\r\nconnection: keep-alive\r\n\r\n")

    def sink():
        srv.settimeout(1.0)
        while not stop.is_set() and time.time() < deadline:
            try:
                conn, _ = srv.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            conn.settimeout(6)
            try:
                while not stop.is_set() and time.time() < deadline:
                    buf = h1._read_until(conn, b"\r\n\r\n")
                    if b"\r\n\r\n" not in buf:
                        break
                    head, _, rest = buf.partition(b"\r\n\r\n")
                    rec = h1._parse(head + b"\r\n\r\n")
                    if h1._is_target(rec):
                        result["target"] = rec
                        stop.set()
                        break
                    cl = h1._content_length(rec["headers"])
                    if cl is not None:
                        remaining = cl - len(rest)
                        while remaining > 0:
                            try:
                                chunk = conn.recv(min(65536, remaining))
                            except socket.timeout:
                                break
                            if not chunk:
                                break
                            remaining -= len(chunk)
                    try:
                        conn.sendall(resp)
                    except OSError:
                        break
                    if cl is None:
                        break
            finally:
                try:
                    conn.close()
                except OSError:
                    pass

    # Seed isolated config (copied creds + pre-trusted cwd, NO hooks), then PTY-fork
    # claude. Socket is bound+listening BEFORE the fork, so connections queue in the
    # backlog; the accept thread starts AFTER the fork to avoid forking with an
    # active accept() thread.
    aoprobe.seed_config([])
    sess = aoprobe.ClaudeSession("hi", f"http://127.0.0.1:{port}", PTY_LOG)
    sess.start()
    t = threading.Thread(target=sink, daemon=True)
    t.start()
    try:
        sess.run(until=lambda: result["target"] is not None, max_s=max_s)
    finally:
        sess.exit()
        stop.set()
        try:
            srv.close()
        except OSError:
            pass
        t.join(timeout=3)
    return result["target"]


def main():
    print("==== interactive vs headless claude HTTP/1.1 header parity ====\n")
    print("[headless] capturing claude -p ...", flush=True)
    hl = headless_target()
    print(f"   {'captured' if hl else 'NO CAPTURE'}")
    print("[interactive] PTY-driving claude (trust dialog auto-answered) ...", flush=True)
    it = interactive_target()
    print(f"   {'captured' if it else 'NO CAPTURE'}\n")

    if not hl or not it:
        print("!! missing a leg — cannot compare "
              f"(headless={bool(hl)} interactive={bool(it)}).")
        return

    hn, in_ = h1.e2e_names(hl), h1.e2e_names(it)
    print(f"  headless    e2e headers: {hn}")
    print(f"  interactive e2e headers: {in_}")
    match = hn == in_
    print(f"\n  identical name+order+casing: {match}")
    if not match:
        only_h = [h for h in hn if h not in in_]
        only_i = [h for h in in_ if h not in hn]
        if only_h:
            print(f"    only in headless    : {only_h}")
        if only_i:
            print(f"    only in interactive : {only_i}")
        if set(hn) == set(in_):
            print("    (same SET, different ORDER — both fetch-sorted, so reorder is "
                  "unexpected; inspect)")

    print("\n==== VERDICT ====")
    if match:
        print("  Interactive claude emits the IDENTICAL application header block as")
        print("  headless (same Bun+SDK fetch serializer). The byte-for-byte h1 match")
        print("  proven against headless (probe_h1_headerforms) applies to the live")
        print("  interactive path AO will tap. No interactive-only header to anticipate.")
    else:
        print("  Interactive differs from headless — see diff above. The proxy still")
        print("  forwards claude's actual headers verbatim, but the integration doc")
        print("  must note the interactive-specific header set.")
    print("================================================================")


if __name__ == "__main__":
    main()
