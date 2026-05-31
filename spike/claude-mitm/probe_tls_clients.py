#!/usr/bin/env python3
"""Follow-up to ja3_diff.py: which Bun HTTP client should the proxy's OUTBOUND leg
use? ja3_diff measured only `fetch()`'s ClientHello; a different client could set
different TLS options (ALPN, OCSP/SCT) and emit a different hello. This measures the
ClientHello of every candidate against the claude baseline, so the choice is
grounded in TLS data — not inferred from fetch alone.

Pairs with probe_h1_headerforms.py, which measures the SAME candidates at the
HTTP/1.1 layer. Together they pick the client. Measured result (live 2026-05-30):

  fetch              web client   — ClientHello MATCHES claude. AND (headerforms)
                                     given a plain Title-Case header object, fetch
                                     reproduces claude's complete h1 block
                                     byte-for-byte.  => THE OUTBOUND CLIENT.
  node:https.request node-compat  — ClientHello MATCHES claude, BUT (headerforms)
                                     node:http/https LOWERCASES header names in
                                     every construction form. Right TLS, wrong h1
                                     => NOT viable.
  raw tls.connect    bare socket  — could write claude's exact h1 bytes, but its
                                     ClientHello DIFFERS: it omits ext 5 (OCSP) and
                                     18 (SCT) that the high-level clients enable;
                                     `requestOCSP:true` does NOT add them => NOT
                                     viable.
  Bun.connect        native tls   — ClientHello DIFFERS (own ext set, adds SNI)
                                     => NOT viable.

Headline: only the HIGH-LEVEL clients (fetch, node:https) reproduce claude's
ClientHello — the OCSP (ext 5) + SCT (ext 18) extensions come from Bun's high-level
TLS setup, not from bare sockets. Of those, only `fetch` also reproduces claude's
h1. So the proxy outbound leg = version-pinned Bun `fetch`, fed claude's headers as
a plain Title-Case object. No raw-socket / uTLS spoofing needed — it GENUINELY IS a
Bun/BoringSSL fetch, identical to the one claude uses.

Note on SNI: measured against 127.0.0.1 (an IP literal), exactly as ja3_diff is,
so server_name (ext 0) is omitted on every leg and the comparison stays
apples-to-apples. Production connects to the hostname api.anthropic.com, which
adds an identical server_name to BOTH claude and the proxy — preserving the match.
"""
import ja3_diff as j

# node:https — ALPN must be set explicitly to match claude's http/1.1 (ext 16).
NODEHTTPS = ("const r=require('node:https').request("
             "{host:'127.0.0.1',port:%d,method:'POST',path:'/v1/messages',"
             "ALPNProtocols:['http/1.1'],rejectUnauthorized:false},()=>{});"
             "r.on('error',()=>{});r.end();")
# raw TLS socket — the byte-passthrough outbound leg.
RAWTLS = ("const s=require('node:tls').connect("
          "{host:'127.0.0.1',port:%d,ALPNProtocols:['http/1.1'],"
          "rejectUnauthorized:false},()=>{});s.on('error',()=>{});")
# raw TLS socket WITHOUT explicit ALPN — to show ALPN is client-set (control).
RAWTLS_NOALPN = ("const s=require('node:tls').connect("
                 "{host:'127.0.0.1',port:%d,rejectUnauthorized:false},()=>{});"
                 "s.on('error',()=>{});")
# raw TLS socket + requestOCSP — does it add the status_request (ext 5) / SCT
# (ext 18) extensions that bare tls.connect omits but claude/fetch/node:https have?
RAWTLS_OCSP = ("const s=require('node:tls').connect("
               "{host:'127.0.0.1',port:%d,ALPNProtocols:['http/1.1'],"
               "requestOCSP:true,rejectUnauthorized:false},()=>{});"
               "s.on('error',()=>{});")
# Bun's NATIVE TLS socket (not the node:tls compat shim) — Bun.connect. This is
# the stack fetch uses; does it emit claude's ClientHello when used directly?
BUNCONNECT = ("Bun.connect({hostname:'127.0.0.1',port:%d,tls:true,"
              "socket:{open(){},data(){},error(){},handshake(){}}})"
              ".catch(()=>{});")


def spawn_bun_eval(template):
    def f(port):
        return j._popen([j.BUN1314, "-e", template % port])
    return f


CLIENTS = [
    ("claude-2.1.158", j.spawn_claude),
    ("bun-1.3.14 fetch", j.spawn_js(j.BUN1314)),
    ("bun-1.3.14 node:https", spawn_bun_eval(NODEHTTPS)),
    ("bun-1.3.14 raw tls.connect (ALPN h1)", spawn_bun_eval(RAWTLS)),
    ("bun-1.3.14 raw tls.connect (no ALPN)", spawn_bun_eval(RAWTLS_NOALPN)),
    ("bun-1.3.14 raw tls.connect (ALPN h1 + requestOCSP)", spawn_bun_eval(RAWTLS_OCSP)),
    ("bun-1.3.14 Bun.connect (native tls)", spawn_bun_eval(BUNCONNECT)),
]


def main():
    print("==== Bun outbound-client ClientHello vs claude (which client to use) ====\n")
    results = {}
    for label, spawn in CLIENTS:
        print(f"[capturing] {label} ...", flush=True)
        parts = j.capture(spawn)
        if not parts:
            print(f"   !! no ClientHello for {label}")
            continue
        results[label] = parts
        print(f"   normalized fp : {j.normalized_fp(parts)}")
        print(f"   ja3 (deGREASE): {j.md5(j.ja3_string(parts, True))}")
        print(f"   ext set       : {sorted(x for x in parts['extensions'] if not j.is_grease(x))}  alpn={parts['alpn']}")
        print()

    if "claude-2.1.158" not in results:
        print("NO claude baseline — cannot compare.")
        return
    base = j.normalized_fp(results["claude-2.1.158"])
    print(f"claude normalized fp = {base}\n---- match vs claude ----")
    verdict = {}
    for label, parts in results.items():
        if label == "claude-2.1.158":
            continue
        m = j.normalized_fp(parts) == base
        verdict[label] = m
        print(f"  {'MATCH ' if m else 'DIFFER'}  {label}")

    fetch_m = verdict.get("bun-1.3.14 fetch", False)
    nodehttps = verdict.get("bun-1.3.14 node:https", False)
    rawtls = verdict.get("bun-1.3.14 raw tls.connect (ALPN h1)", False)
    print("\n---- headline ----")
    print(f"  fetch      reproduces claude's ClientHello: {fetch_m}"
          "   <- AND reproduces h1 (probe_h1_headerforms) => THE outbound client")
    print(f"  node:https reproduces claude's ClientHello: {nodehttps}"
          "   <- but LOWERCASES h1 (probe_h1_headerforms) => not viable")
    print(f"  raw tls.connect (ALPN h1) reproduces it:    {rawtls}"
          "   <- omits ext 5/18, even with requestOCSP => not viable")
    print("\nOnly the high-level clients carry OCSP(5)+SCT(18); only fetch also "
          "reproduces claude's h1. => outbound leg = version-pinned Bun fetch.")
    print("========================================================================")


if __name__ == "__main__":
    main()
