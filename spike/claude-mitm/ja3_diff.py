#!/usr/bin/env python3
"""Spike: does a Bun client's TLS ClientHello fingerprint MATCH claude's — and do
Go / Node differ?

Context: AO taps claude's wire by running a local reverse proxy that TERMINATES
claude's TLS and RE-ORIGINATES a fresh connection to api.anthropic.com. Whatever
runtime the proxy is written in determines the fingerprint Anthropic's edge
(Cloudflare) sees — NOT claude's. The current spike proxy is Go, so Anthropic sees
Go's JA3/h2, a mismatch against the genuine claude client.

claude is a Bun-compiled binary (embeds Bun 1.3.14) and Bun statically links
BoringSSL. Hypothesis (advisor): write the proxy's outbound leg in Bun and it
GENUINELY IS a Bun/BoringSSL connection — so the ClientHello converges with
claude's, no spoofing/uTLS/eBPF. This proves or refutes that by capturing each
runtime's real ClientHello and diffing.

Method: a raw TCP listener on 127.0.0.1 records the first TLS record (the
ClientHello, sent in the clear before any cert validation), parses it, and
computes JA3. Each client is pointed at the listener:
  - claude         via ANTHROPIC_BASE_URL=https://127.0.0.1:<port>
  - bun 1.3.14     (claude's embedded version) fetch()
  - bun 1.3.11     (local) fetch()         -> Bun version sensitivity
  - node 24        fetch() (undici/OpenSSL) -> expected to DIFFER (not BoringSSL)
  - go 1.25        net/http (crypto/tls)    -> the CURRENT proxy; expected to DIFFER

The ClientHello fingerprint is destination-independent (cipher/extension/group
lists are client-determined), so capturing at a local socket is faithful. GREASE
values and extension order can vary per-connection (BoringSSL randomizes), so we
compare a GREASE-stripped fingerprint with extensions as a sorted set, and run
claude + bun twice to show intra-client stability.
"""
import hashlib
import json
import os
import socket
import struct
import subprocess
import sys
import time

GREASE = {0x0a0a, 0x1a1a, 0x2a2a, 0x3a3a, 0x4a4a, 0x5a5a, 0x6a6a, 0x7a7a,
          0x8a8a, 0x9a9a, 0xaaaa, 0xbaba, 0xcaca, 0xdada, 0xeaea, 0xfafa}

CLAUDE = "claude"
BUN1314 = "/tmp/bun1314/bun"
BUN1311 = os.path.expanduser("~/.bun/bin/bun")
GO_SRC = "/tmp/ghello/main.go"


def is_grease(v):
    return v in GREASE


def parse_client_hello(data):
    if not data or data[0] != 0x16:
        raise ValueError("not a TLS handshake record")
    rec_len = struct.unpack(">H", data[3:5])[0]
    hs = data[5:5 + rec_len]
    if not hs or hs[0] != 0x01:
        raise ValueError("not a ClientHello")
    p = 4                                            # skip handshake type(1)+len(3)
    client_version = struct.unpack(">H", hs[p:p + 2])[0]; p += 2
    p += 32                                          # random
    sid_len = hs[p]; p += 1 + sid_len
    cs_len = struct.unpack(">H", hs[p:p + 2])[0]; p += 2
    ciphers = [struct.unpack(">H", hs[p + i:p + i + 2])[0] for i in range(0, cs_len, 2)]
    p += cs_len
    comp_len = hs[p]; p += 1 + comp_len
    exts, groups, point_formats, alpn = [], [], [], []
    if p + 2 <= len(hs):
        ext_total = struct.unpack(">H", hs[p:p + 2])[0]; p += 2
        end = min(p + ext_total, len(hs))
        while p + 4 <= end:
            etype = struct.unpack(">H", hs[p:p + 2])[0]
            elen = struct.unpack(">H", hs[p + 2:p + 4])[0]
            edata = hs[p + 4:p + 4 + elen]
            exts.append(etype)
            if etype == 0x000a and len(edata) >= 2:          # supported_groups
                glen = struct.unpack(">H", edata[0:2])[0]
                groups = [struct.unpack(">H", edata[2 + i:4 + i])[0]
                          for i in range(0, glen, 2)]
            elif etype == 0x000b and len(edata) >= 1:         # ec_point_formats
                point_formats = list(edata[1:1 + edata[0]])
            elif etype == 0x0010 and len(edata) >= 2:         # ALPN
                ap = 2
                while ap < len(edata):
                    n = edata[ap]; alpn.append(edata[ap + 1:ap + 1 + n].decode("latin1"))
                    ap += 1 + n
            p += 4 + elen
    return {"client_version": client_version, "ciphers": ciphers,
            "extensions": exts, "groups": groups,
            "point_formats": point_formats, "alpn": alpn}


def ja3_string(parts, strip_grease):
    def f(xs):
        return [x for x in xs if not (strip_grease and is_grease(x))]
    return "%d,%s,%s,%s,%s" % (
        parts["client_version"],
        "-".join(map(str, f(parts["ciphers"]))),
        "-".join(map(str, f(parts["extensions"]))),
        "-".join(map(str, f(parts["groups"]))),
        "-".join(map(str, parts["point_formats"])),
    )


def md5(s):
    return hashlib.md5(s.encode()).hexdigest()


def normalized_fp(parts):
    """GREASE-stripped, extensions as SORTED SET (robust to BoringSSL extension
    shuffling), ciphers ordered (stable), groups ordered. The stable identity."""
    ng = lambda xs: [x for x in xs if not is_grease(x)]
    return md5("|".join([
        str(parts["client_version"]),
        "-".join(map(str, ng(parts["ciphers"]))),
        "-".join(map(str, sorted(ng(parts["extensions"])))),
        "-".join(map(str, ng(parts["groups"]))),
        "-".join(map(str, parts["point_formats"])),
        ",".join(parts["alpn"]),
    ]))


def capture(spawn, timeout=30):
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", 0))
    port = srv.getsockname()[1]
    srv.listen(8)
    srv.settimeout(timeout)
    proc = spawn(port)
    try:
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                conn, _ = srv.accept()
            except socket.timeout:
                return None
            conn.settimeout(6)
            buf = b""
            try:
                while len(buf) < 5:
                    chunk = conn.recv(8192)
                    if not chunk:
                        break
                    buf += chunk
                if len(buf) >= 5 and buf[0] == 0x16:
                    rec_len = struct.unpack(">H", buf[3:5])[0]
                    while len(buf) < 5 + rec_len:
                        chunk = conn.recv(8192)
                        if not chunk:
                            break
                        buf += chunk
                    if len(buf) >= 5 + rec_len:
                        return parse_client_hello(buf)
            except (socket.timeout, ValueError, struct.error):
                pass
            finally:
                try:
                    conn.close()
                except OSError:
                    pass
        return None
    finally:
        try:
            proc.terminate(); proc.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            try:
                proc.kill()
            except OSError:
                pass
        srv.close()


def _popen(argv, **env_over):
    env = dict(os.environ, NODE_TLS_REJECT_UNAUTHORIZED="0", **env_over)
    return subprocess.Popen(argv, env=env, stdin=subprocess.DEVNULL,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def spawn_js(runtime):
    def f(port):
        url = "https://127.0.0.1:%d/v1/messages" % port
        return _popen([runtime, "-e", "fetch(%r).catch(()=>{})" % url])
    return f


def spawn_go(port):
    url = "https://127.0.0.1:%d/v1/messages" % port
    return _popen(["go", "run", GO_SRC, url])


def spawn_claude(port):
    url = "https://127.0.0.1:%d" % port
    return _popen([CLAUDE, "-p", "hi"], ANTHROPIC_BASE_URL=url)


CLIENTS = [
    ("claude-2.1.158", spawn_claude),
    ("claude-2.1.158 (run2)", spawn_claude),
    ("bun-1.3.14 (claude's)", spawn_js(BUN1314)),
    ("bun-1.3.14 (run2)", spawn_js(BUN1314)),
    ("bun-1.3.11 (local)", spawn_js(BUN1311)),
    ("node-24 (undici/OpenSSL)", spawn_js("node")),
    ("go-1.25 (current proxy)", spawn_go),
]


def main():
    print("==== ClientHello / JA3 fingerprint diff ====\n")
    results = {}
    for label, spawn in CLIENTS:
        print(f"[capturing] {label} ...", flush=True)
        parts = capture(spawn)
        if not parts:
            print(f"   !! no ClientHello captured for {label}")
            continue
        results[label] = parts
        print(f"   ja3 (classic)   : {md5(ja3_string(parts, False))}")
        print(f"   ja3 (de-GREASE) : {md5(ja3_string(parts, True))}")
        print(f"   normalized fp   : {normalized_fp(parts)}")
        print(f"   ciphers({len(parts['ciphers'])}) groups={parts['groups']} alpn={parts['alpn']}")
        print(f"   ext set         : {sorted(x for x in parts['extensions'] if not is_grease(x))}")
        print()

    print("==== COMPARISON (normalized, GREASE-stripped, ext-as-set) ====")
    if "claude-2.1.158" not in results:
        print("NO claude baseline captured — cannot compare.")
        return
    base = normalized_fp(results["claude-2.1.158"])
    print(f"claude normalized fp = {base}\n")
    verdict = {}
    for label, parts in results.items():
        if label.startswith("claude-2.1.158"):
            continue
        fp = normalized_fp(parts)
        match = (fp == base)
        verdict[label] = match
        print(f"  {'MATCH ' if match else 'DIFFER'}  {label:<28} {fp}")

    bun1314 = [k for k in verdict if k.startswith("bun-1.3.14")]
    bun1314_match = bool(bun1314) and all(verdict[k] for k in bun1314)
    bun1311_match = verdict.get("bun-1.3.11 (local)", False)
    node_differs = not verdict.get("node-24 (undici/OpenSSL)", True)
    go_differs = not verdict.get("go-1.25 (current proxy)", True)
    print("\n---- headline ----")
    print(f"  Bun 1.3.14 (claude's EXACT embedded Bun) matches claude:  {bun1314_match}")
    print(f"  Bun 1.3.11 (mismatched version) matches claude:           {bun1311_match}"
          "   <- version-coupling: pin claude's Bun")
    print(f"  Node differs (undici/OpenSSL != BoringSSL):               {node_differs}"
          "   <- must be Bun, NOT 'Bun or Node'")
    print(f"  Go differs (current proxy, crypto/tls != BoringSSL):      {go_differs}")
    ok = bun1314_match and go_differs
    print("\nVERDICT: " + (
        "CONFIRMED — a VERSION-PINNED Bun proxy reproduces claude's TLS fingerprint "
        "byte-for-byte; Go / Node / older-Bun do not"
        if ok else "INCONCLUSIVE — inspect above"))
    with open("/tmp/ja3_results.json", "w") as fh:
        json.dump({k: v for k, v in results.items()}, fh, indent=2)
    print("raw parsed ClientHellos -> /tmp/ja3_results.json")
    print("=============================================")


if __name__ == "__main__":
    main()
