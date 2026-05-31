#!/usr/bin/env python3
"""Spike: which Bun forwarder reproduces claude's HTTP/1.1 request serialization
(header names / casing / order) BYTE-FOR-BYTE — `fetch` or `node:http`?

WHY this exists (the gap ja3_diff.py does NOT close). ja3_diff proved the TLS
ClientHello of `bun-1.3.14` is byte-identical to claude's. But the ClientHello is
a *BoringSSL-layer* artifact, and EVERY Bun HTTP client shares the same BoringSSL +
default SSL_CTX — so a matching ClientHello proves claude uses Bun's default TLS
config, NOT that claude's HTTP client is `fetch`. The HTTP/1.1 header serialization
(lowercasing, ordering, auto-headers, framing) is a SEPARATE code path that must be
MEASURED, not assumed.

We measure OUTPUT, not runtime identity (robust to whichever client claude uses):

  (direct)   claude ─http─> raw TCP sink                      # claude's native h1
  (fetch)    claude ─http─> Bun.serve + fetch fwd ─http─> sink # web-client re-serialize
  (nodehttp) claude ─http─> raw TCP + node:http fwd ─http─> sink # node-client, casing/order preserved

All legs are plain `http://` (ANTHROPIC_BASE_URL), so the sink reads cleartext
HTTP/1.1 directly — no TLS, no cert. TLS parity is proven separately by ja3_diff /
probe_tls_clients; this isolates layer 7. We diff each forwarder's POST
/v1/messages header block against claude's.

ACTUAL RESULT (live 2026-05-30) — BOTH forwarders DIFFER, for DIFFERENT reasons.
This probe's forwarder design CONFOUNDS the fetch leg; do not read its fetch verdict
as fetch's real behaviour. It is SUPERSEDED by probe_h1_headerforms.py (clean,
direct, no forwarder), which is the authoritative h1 measurement.
  - fetch    DIFFERS here, but this is a CONFOUND: the forwarder feeds fetch
             `req.headers` from Bun.serve, which LOWERCASES header names on INGEST,
             so fetch never sees claude's Title-Case names and faithfully preserves
             the already-lowercased ones. Tested cleanly (no Bun.serve, a plain
             Title-Case object) in probe_h1_headerforms.py, Bun fetch reproduces
             claude's COMPLETE wire block byte-for-byte. The real architecture
             lesson from this leg: the proxy must NOT normalise header casing on
             ingest (don't use a Bun.serve Headers object as the source of truth).
  - nodehttp DIFFERS for real: node:http LOWERCASES header names in EVERY
             construction form (setHeader / headers-object / array), confirmed
             directly in probe_h1_headerforms.py. node:https shares this code path,
             so despite matching the ClientHello (probe_tls_clients.py) it CANNOT
             reproduce claude's h1 — it is NOT the outbound client. `fetch` is.

The verdict keys on END-TO-END (application) headers' name+casing+order — the
client-identifying surface. HOP-BY-HOP / framing headers (Host, Connection,
Content-Length, Transfer-Encoding, Accept-Encoding) are per-leg and
proxy-controllable; reported for awareness, not gating.

SECURITY: captured requests carry claude's real OAuth bearer. Credential header
VALUES (authorization / cookie / x-api-key / *-token) are redacted to
`<redacted len=N>` before anything is printed or written. Names/order/casing —
the only thing this probe studies — are preserved.
"""
import json
import os
import socket
import subprocess
import tempfile
import time

CLAUDE = "claude"
BUN1314 = "/tmp/bun1314/bun"

# Hop-by-hop + body-framing headers: per-leg, set by whichever client originates
# the connection, controllable by the real proxy. Excluded from the match verdict.
FRAMING = {"host", "connection", "content-length", "transfer-encoding",
           "accept-encoding", "keep-alive", "proxy-connection", "te", "upgrade"}

SECRET = ("authorization", "proxy-authorization", "x-api-key", "cookie",
          "set-cookie")


def is_secret(name):
    n = name.lower()
    return (n in SECRET or "api-key" in n or "access-token" in n
            or "refresh-token" in n or "session-token" in n or "auth-token" in n)


def redact(name, value):
    return f"<redacted len={len(value)}>" if is_secret(name) else value


# ---------------------------------------------------------------------------
# Raw HTTP/1.1 sink: capture verbatim header blocks (keep-alive aware), reply
# minimally. Early-exits once the target POST /v1/messages is captured.
# ---------------------------------------------------------------------------
def _read_until(conn, marker, cap=262144):
    buf = b""
    while marker not in buf and len(buf) < cap:
        try:
            chunk = conn.recv(8192)
        except socket.timeout:
            break
        if not chunk:
            break
        buf += chunk
    return buf


def _parse(raw_head):
    text = raw_head.decode("latin1")
    lines = text.split("\r\n")
    request_line = lines[0] if lines else ""
    headers = []
    for ln in lines[1:]:
        if ln == "":
            break
        if ":" in ln:
            name, value = ln.split(":", 1)
            headers.append((name, value.strip()))
    parts = request_line.split(" ")
    method = parts[0] if parts else ""
    path = parts[1] if len(parts) > 1 else ""
    return {"method": method, "path": path, "request_line": request_line,
            "headers": [(n, redact(n, v)) for n, v in headers]}


def _content_length(headers):
    for n, v in headers:
        if n.lower() == "content-length":
            try:
                return int(v)
            except ValueError:
                return 0
    return None


def _is_target(rec):
    return rec["method"] == "POST" and rec["path"].startswith("/v1/messages")


def capture(spawn, deadline_s=40):
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", 0))
    port = srv.getsockname()[1]
    srv.listen(16)
    srv.settimeout(min(deadline_s, 12))
    proc = spawn(port)
    captured = []
    got_target = False
    resp = b"HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ncontent-length: 0\r\nconnection: keep-alive\r\n\r\n"
    deadline = time.time() + deadline_s
    try:
        while time.time() < deadline and not got_target:
            try:
                conn, _ = srv.accept()
            except socket.timeout:
                break
            conn.settimeout(6)
            try:
                # Keep-alive: read sequential requests on this connection.
                while time.time() < deadline:
                    buf = _read_until(conn, b"\r\n\r\n")
                    if b"\r\n\r\n" not in buf:
                        break
                    head, _, rest = buf.partition(b"\r\n\r\n")
                    rec = _parse(head + b"\r\n\r\n")
                    captured.append(rec)
                    if _is_target(rec):
                        got_target = True
                        break  # have what we need; skip drain/respond, tear down
                    # Drain body so the next pipelined request aligns.
                    cl = _content_length(rec["headers"])
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
                        break  # can't realign (e.g. chunked); one req per conn
            finally:
                try:
                    conn.close()
                except OSError:
                    pass
    finally:
        try:
            proc.terminate(); proc.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            try:
                proc.kill()
            except OSError:
                pass
        srv.close()
    return captured


# ---------------------------------------------------------------------------
# Spawners
# ---------------------------------------------------------------------------
def _popen(argv, **env_over):
    env = dict(os.environ, NODE_TLS_REJECT_UNAUTHORIZED="0", **env_over)
    return subprocess.Popen(argv, env=env, stdin=subprocess.DEVNULL,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def spawn_claude_to(port):
    """claude -p, pointed straight at the sink (captures claude's NATIVE h1)."""
    return _popen([CLAUDE, "-p", "hi"],
                  ANTHROPIC_BASE_URL=f"http://127.0.0.1:{port}")


# Forwarder A: Bun.serve + fetch (the naive "re-serialize through fetch" proxy).
FETCH_FWD_JS = r"""
const SINK = process.env.SINK;
const PORT = Number(process.env.PORT);
Bun.serve({
  port: PORT, hostname: "127.0.0.1",
  async fetch(req) {
    const u = new URL(req.url);
    const target = SINK + u.pathname + (u.search || "");
    const init = { method: req.method, headers: req.headers, redirect: "manual" };
    if (req.method !== "GET" && req.method !== "HEAD") init.body = await req.arrayBuffer();
    try { const r = await fetch(target, init); return new Response(await r.arrayBuffer(), { status: r.status }); }
    catch (e) { return new Response("fwd-err", { status: 502 }); }
  },
});
"""

# Forwarder B: raw TCP listener + node:http, preserving claude's RAW header
# order+casing via setHeader() in wire order. Reads raw bytes (NOT Bun.serve's
# Headers object), so casing/order survive. Flushes headers immediately; the body
# is irrelevant to the header-block comparison.
NODEHTTP_FWD_JS = r"""
const net = require("node:net");
const http = require("node:http");
const SINK = new URL(process.env.SINK);
const PORT = Number(process.env.PORT);
net.createServer((sock) => {
  let buf = Buffer.alloc(0), done = false;
  sock.on("data", (d) => {
    if (done) return;
    buf = Buffer.concat([buf, d]);
    const idx = buf.indexOf("\r\n\r\n");
    if (idx === -1) return;
    done = true;
    const head = buf.slice(0, idx).toString("latin1").split("\r\n");
    const reqline = head[0].split(" ");
    const req = http.request({ host: SINK.hostname, port: Number(SINK.port),
                               method: reqline[0], path: reqline[1] });
    for (const ln of head.slice(1)) {
      const ci = ln.indexOf(":"); if (ci === -1) continue;
      const name = ln.slice(0, ci); const value = ln.slice(ci + 1).replace(/^\s+/, "");
      if (name.toLowerCase() === "host") continue;   // node sets its own Host
      try { req.setHeader(name, value); } catch (e) {}
    }
    req.on("error", () => {});
    req.on("response", (r) => { r.resume(); });
    req.flushHeaders();
  });
  sock.on("error", () => {});
}).listen(PORT, "127.0.0.1");
"""


def spawn_via_forwarder(js):
    """Write a Bun forwarder script, then return a spawn(sink_port) that boots the
    forwarder (SINK+PORT env) and points claude at it. capture() supplies the live
    sink port."""
    fwd_dir = tempfile.mkdtemp(prefix="h1fwd-")
    fwd_path = os.path.join(fwd_dir, "fwd.js")
    with open(fwd_path, "w") as fh:
        fh.write(js)

    def spawn(sink_port):
        s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        s.bind(("127.0.0.1", 0))
        fwd_port = s.getsockname()[1]
        s.close()
        fwd_proc = _popen([BUN1314, fwd_path],
                          SINK=f"http://127.0.0.1:{sink_port}", PORT=str(fwd_port))
        time.sleep(1.2)  # let the forwarder bind
        claude_proc = _popen([CLAUDE, "-p", "hi"],
                             ANTHROPIC_BASE_URL=f"http://127.0.0.1:{fwd_port}")

        class Pair:
            def terminate(self):
                for p in (claude_proc, fwd_proc):
                    try:
                        p.terminate()
                    except OSError:
                        pass

            def wait(self, timeout=None):
                for p in (claude_proc, fwd_proc):
                    try:
                        p.wait(timeout=timeout)
                    except (OSError, subprocess.TimeoutExpired):
                        pass

            def kill(self):
                for p in (claude_proc, fwd_proc):
                    try:
                        p.kill()
                    except OSError:
                        pass
        return Pair()
    return spawn


# ---------------------------------------------------------------------------
# Diff
# ---------------------------------------------------------------------------
def pick_target(reqs):
    posts = [r for r in reqs if _is_target(r)]
    return posts[0] if posts else None


def e2e_names(rec):
    return [n for n, _ in rec["headers"] if n.lower() not in FRAMING]


def framing_pairs(rec):
    return [(n, v) for n, v in rec["headers"] if n.lower() in FRAMING]


def diff_report(direct, via, label):
    d = pick_target(direct)
    v = pick_target(via)
    if not d or not v:
        print(f"\n[{label}] !! missing POST /v1/messages on one leg "
              f"(direct={bool(d)} via={bool(v)}); inspect.")
        return None
    dn, vn = e2e_names(d), e2e_names(v)
    match = (dn == vn)
    print(f"\n==== [{label}] vs claude-native ====")
    print(f"  claude-native  e2e headers: {dn}")
    print(f"  {label:<13} e2e headers: {vn}")
    print(f"  -> identical name+order+casing: {match}")
    if not match:
        only_d = [h for h in dn if h not in vn]
        only_v = [h for h in vn if h not in dn]
        if only_d:
            print(f"     only in claude : {only_d}")
        if only_v:
            print(f"     only in {label} : {only_v}")
        shared_d = [h for h in dn if h in vn]
        shared_v = [h for h in vn if h in dn]
        if shared_d != shared_v:
            print(f"     reordered/recased:\n       claude: {shared_d}\n       {label}: {shared_v}")
    print(f"  framing claude : {framing_pairs(d)}")
    print(f"  framing {label:<6}: {framing_pairs(v)}")
    return match


def main():
    print("==== HTTP/1.1 SERIALIZATION DIFF (claude-native vs Bun forwarders) ====")
    if not os.path.exists(BUN1314):
        print(f"!! {BUN1314} missing — download bun 1.3.14 first (see ja3_diff.py).")
        return

    print("\n[capture] claude -> sink (native h1)...", flush=True)
    direct = capture(spawn_claude_to)
    print(f"   {[(r['method'], r['path'][:24]) for r in direct]}")

    print("\n[capture] claude -> Bun.serve+fetch forwarder -> sink...", flush=True)
    via_fetch = capture(spawn_via_forwarder(FETCH_FWD_JS))
    print(f"   {[(r['method'], r['path'][:24]) for r in via_fetch]}")

    print("\n[capture] claude -> node:http forwarder (raw, preserves order/casing) -> sink...", flush=True)
    via_node = capture(spawn_via_forwarder(NODEHTTP_FWD_JS))
    print(f"   {[(r['method'], r['path'][:24]) for r in via_node]}")

    m_fetch = diff_report(direct, via_fetch, "fetch")
    m_node = diff_report(direct, via_node, "nodehttp")

    print("\n---- VERDICT ----")
    print(f"  fetch    reproduces claude's h1 header signature: {m_fetch}")
    print(f"  nodehttp reproduces claude's h1 header signature: {m_node}")
    if m_node and not m_fetch:
        print("  => Use node:https for the outbound leg (preserves claude's raw header\n"
              "     order+casing) — and probe_tls_clients shows node:https also matches\n"
              "     claude's ClientHello. Both layers covered, fully measured.")
    elif m_fetch:
        print("  => fetch also matches — simplest option.")
    else:
        print("  => neither matched; inspect the diffs above.")

    with open("/tmp/h1_serialize_results.json", "w") as fh:
        json.dump({"direct": direct, "via_fetch": via_fetch, "via_node": via_node},
                  fh, indent=2)
    print("raw captured header blocks (creds redacted) -> /tmp/h1_serialize_results.json")
    print("=====================================================================")


if __name__ == "__main__":
    main()
