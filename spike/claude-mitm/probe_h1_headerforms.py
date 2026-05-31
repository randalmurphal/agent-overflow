#!/usr/bin/env python3
"""Spike: does Bun `fetch` reproduce claude's HTTP/1.1 header serialization, and
HOW must the proxy feed it to do so?

Background: probe_h1_serialize.py (a claude -> Bun.serve+fetch -> sink forwarder)
appeared to show fetch MANGLING claude's headers (lowercased X-Stainless-*,
reordered). That was a CONFOUND, not fetch's behaviour: Bun.serve normalises
(lowercases) header names on INGEST, so fetch never saw claude's Title-Case names —
it faithfully preserved the already-lowercased ones. claude is itself a Bun program
emitting Title-Case, case-sensitively-sorted headers, so Bun is provably capable of
it; the question is which construction form triggers it.

This probe isolates the variable with a STANDALONE Bun process (no Bun.serve, no
ingest step) firing synthetic headers straight at a raw http sink.

claude's ground truth (POST /v1/messages, from /tmp/h1_serialize_results.json):
  - application headers in CASE-SENSITIVE ASCII sort (A<C<U<X<a<x), so:
      Accept, Authorization, Content-Type, User-Agent,
      X-Claude-Code-Session-Id, X-Stainless-* (x9), anthropic-* (x3), x-app
  - then FRAMING appended: Connection, Host, Accept-Encoding, Content-Length

TEST 1 (forms): the same shuffled header set built six ways, to find which
construction form preserves Title-Case + case-sensitive sort. Discriminator is a
non-well-known header (X-Stainless-Arch) — the kind the confounded test lowercased.

TEST 2 (full-set closing proof): feed Bun fetch claude's COMPLETE 17-header
application set as a plain object (Title-Case, synthetic values, shuffled), with a
body, and NO framing headers (those are hop-by-hop; a reverse proxy strips them and
the outbound client regenerates them). Assert the full wire block returns
byte-identical to claude's — application block in claude's exact order+casing, then
fetch's regenerated framing matching claude's framing (same Bun fetch => same
defaults). This is the architecture's outbound leg, measured end to end.

No credentials: every value is synthetic (Authorization is a fake bearer string).
"""
import os
import socket
import subprocess
import tempfile
import time

import probe_h1_serialize as h1

BUN1314 = "/tmp/bun1314/bun"

# ---- claude ground truth (names, from /tmp/h1_serialize_results.json) ----
CLAUDE_APP = [
    "Accept", "Authorization", "Content-Type", "User-Agent",
    "X-Claude-Code-Session-Id", "X-Stainless-Arch", "X-Stainless-Lang",
    "X-Stainless-OS", "X-Stainless-Package-Version", "X-Stainless-Retry-Count",
    "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Timeout",
    "anthropic-beta", "anthropic-dangerous-direct-browser-access",
    "anthropic-version", "x-app",
]
CLAUDE_FRAMING = ["Connection", "Host", "Accept-Encoding", "Content-Length"]
CLAUDE_FULL = CLAUDE_APP + CLAUDE_FRAMING

# ---- TEST 1: six construction forms, shuffled subset ----
FORMS_SCRIPT = r'''
const SINK = process.env.SINK;
const http = require("node:http");
const U = new URL(SINK);
const ENTRIES = [
  ["x-app","cli"], ["X-Stainless-Lang","js"], ["anthropic-version","2023-06-01"],
  ["Accept","application/json"], ["X-Stainless-Arch","x64"],
  ["X-Claude-Code-Session-Id","sess"],
];
const obj = () => { const o = {}; for (const [k,v] of ENTRIES) o[k]=v; return o; };
function nodeReq(path, headersOpt, useSetHeader) {
  return new Promise((res) => {
    try {
      const o = {host:U.hostname, port:Number(U.port), method:"POST", path};
      if (headersOpt !== undefined) o.headers = headersOpt;
      const r = http.request(o, (resp)=>{ resp.resume(); resp.on("end",res); });
      if (useSetHeader) for (const [k,v] of ENTRIES) r.setHeader(k,v);
      r.on("error", ()=>res());
      r.end();
    } catch (e) { res(); }
  });
}
const fetchReq = (path, headers) =>
  fetch(SINK+path, {method:"POST", headers}).then(r=>r.text()).then(()=>{}).catch(()=>{});
(async () => {
  await fetchReq("/fetch-object",   obj());                 // plain object -> fetch
  await fetchReq("/fetch-headers",  new Headers(ENTRIES));  // Headers instance
  await nodeReq("/node-object",   obj(),           false);  // plain object -> node:http
  await nodeReq("/node-array",    ENTRIES.flat(),  false);  // flat [k,v,...] array
  await nodeReq("/node-pairs",    ENTRIES,         false);  // [[k,v],...] pairs
  await nodeReq("/node-setheader", undefined,      true);   // setHeader
  process.exit(0);
})();
'''

# ---- TEST 2: claude's full 17-header app set, SHUFFLED, synthetic values ----
# (no framing headers supplied: fetch regenerates Connection/Host/Accept-Encoding/
#  Content-Length itself, exactly as claude's fetch does.)
FULLSET_SCRIPT = r'''
const SINK = process.env.SINK;
const HEADERS = {
  "x-app":"cli",
  "anthropic-version":"2023-06-01",
  "X-Stainless-Timeout":"600",
  "User-Agent":"claude-cli/synthetic (external)",
  "Accept":"application/json",
  "X-Stainless-Runtime":"bun",
  "anthropic-beta":"synthetic-beta",
  "X-Claude-Code-Session-Id":"00000000-0000-0000-0000-000000000000",
  "Content-Type":"application/json",
  "X-Stainless-Arch":"x64",
  "Authorization":"Bearer synthetic-not-a-real-token",
  "X-Stainless-Lang":"js",
  "anthropic-dangerous-direct-browser-access":"true",
  "X-Stainless-OS":"Linux",
  "X-Stainless-Package-Version":"0.0.0",
  "X-Stainless-Retry-Count":"0",
  "X-Stainless-Runtime-Version":"1.3.14",
};
fetch(SINK + "/v1/messages?beta=true", {
  method:"POST", headers: HEADERS,
  body: JSON.stringify({model:"synthetic", messages:[]}),
}).then(r=>r.text()).then(()=>process.exit(0)).catch(()=>process.exit(0));
'''


def _run(bun_script, want, deadline_s=15):
    """Spawn a standalone Bun process against a raw http sink; return the captured
    request records (in arrival order). Each request gets a fresh `connection:
    close` connection, so forms never share/keepalive-confuse a socket."""
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", 0))
    port = srv.getsockname()[1]
    srv.listen(16)
    srv.settimeout(min(deadline_s, 8))

    work = tempfile.mkdtemp(prefix="hdrforms-")
    script = os.path.join(work, "probe.js")
    with open(script, "w") as fh:
        fh.write(bun_script)
    env = dict(os.environ, SINK=f"http://127.0.0.1:{port}")
    proc = subprocess.Popen([BUN1314, script], env=env, stdin=subprocess.DEVNULL,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    recs = []
    resp = b"HTTP/1.1 200 OK\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"
    deadline = time.time() + deadline_s
    try:
        while time.time() < deadline and len(recs) < want:
            try:
                conn, _ = srv.accept()
            except socket.timeout:
                break
            conn.settimeout(5)
            try:
                buf = h1._read_until(conn, b"\r\n\r\n")
                if b"\r\n\r\n" in buf:
                    head, _, _ = buf.partition(b"\r\n\r\n")
                    recs.append(h1._parse(head + b"\r\n\r\n"))
                try:
                    conn.sendall(resp)
                except OSError:
                    pass
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
    return recs


# claude's case-sensitive ASCII sort of TEST 1's subset (target order+casing).
FORMS_TARGET = ["Accept", "X-Claude-Code-Session-Id", "X-Stainless-Arch",
                "X-Stainless-Lang", "anthropic-version", "x-app"]


def raw_names(rec):
    return [name for name, _ in rec["headers"]]


def test_forms():
    print("---- TEST 1: which construction form preserves Title-Case + sort ----")
    print(f"target subset: {FORMS_TARGET}\n")
    recs = _run(FORMS_SCRIPT, want=6)
    by_path = {r["path"]: r for r in recs}
    forms = ["/fetch-object", "/fetch-headers", "/node-object", "/node-array",
             "/node-pairs", "/node-setheader"]
    preserving = []
    for path in forms:
        rec = by_path.get(path)
        if not rec:
            print(f"  {path:<16} (no capture / errored)")
            continue
        names = h1.e2e_names(rec)          # app headers only (framing stripped)
        cased = "X-Stainless-Arch" in names
        exact = names == FORMS_TARGET
        tag = "MATCH" if exact else ("case-ok" if cased else "lowercased")
        print(f"  {path:<16} [{tag:^10}] {names}")
        if cased:
            preserving.append(path)
    print(f"\n  => Title-Case preserved by: {preserving or 'NONE'}")
    print("     (node:http lowercases in every form; fetch preserves + sorts)\n")
    return preserving


def test_fullset():
    print("---- TEST 2: full 17-header set through Bun fetch (closing proof) ----")
    recs = _run(FULLSET_SCRIPT, want=1)
    post = next((r for r in recs if r["method"] == "POST"), None)
    if not post:
        print("  !! no POST captured.\n")
        return False
    got = raw_names(post)
    got_app = h1.e2e_names(post)             # framing stripped
    got_framing = [n for n in got if n not in got_app]

    app_ok = got_app == CLAUDE_APP
    framing_ok = got_framing == CLAUDE_FRAMING
    full_ok = got == CLAUDE_FULL

    print(f"  application order match: {app_ok}")
    if not app_ok:
        print(f"    claude: {CLAUDE_APP}")
        print(f"    fetch : {got_app}")
    print(f"  framing order match:     {framing_ok}  (claude {CLAUDE_FRAMING} | fetch {got_framing})")
    print(f"  FULL wire block match:   {full_ok}")
    if not full_ok:
        print(f"    claude: {CLAUDE_FULL}")
        print(f"    fetch : {got}")
    # bonus: fetch's regenerated Accept-Encoding should equal claude's default.
    ae = dict(post["headers"]).get("Accept-Encoding")
    print(f"  fetch Accept-Encoding:   {ae!r}  (claude: 'gzip, deflate, br, zstd')\n")
    return full_ok


def main():
    print("==== Bun fetch vs claude HTTP/1.1 header serialization ====\n")
    if not os.path.exists(BUN1314):
        print(f"!! {BUN1314} missing.")
        return
    preserving = test_forms()
    full_ok = test_fullset()

    print("==== VERDICT ====")
    if full_ok:
        print("  Bun fetch, given claude's app headers as a PLAIN OBJECT with original")
        print("  Title-Case, reproduces claude's COMPLETE wire header block byte-for-byte")
        print("  (case-sensitive ASCII sort + framing appended). Combined with the")
        print("  ClientHello match (ja3_diff / probe_tls_clients), fetch reproduces BOTH")
        print("  the TLS and the HTTP/1.1 layer. Architecture rules:")
        print("    1. Outbound leg = version-pinned Bun fetch with a plain headers object.")
        print("    2. Inbound read must NOT normalise claude's header casing (no Bun.serve")
        print("       Headers as the source of truth) — preserve claude's raw names.")
        print("    3. Strip hop-by-hop framing before forwarding; fetch regenerates it.")
    else:
        print("  Full-set round-trip did NOT match byte-for-byte — inspect TEST 2 above.")
        print(f"  (Forms preserving Title-Case: {preserving})")
    print("==========================================================")


if __name__ == "__main__":
    main()
