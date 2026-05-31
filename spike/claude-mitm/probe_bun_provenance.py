#!/usr/bin/env python3
"""Keystone integration check: where does the fingerprint-matching Bun come from —
a STOCK download, or must it be extracted from the claude binary?

ja3_diff.py proved a pinned Bun 1.3.14 reproduces claude's ClientHello. But it used
a local binary of unrecorded provenance. If AO must *carve Bun out of claude*, the
integration story is heavy (extraction step, depends on claude's internal layout).
If a plain `bun.sh`/GitHub-releases download matches, integration just pins a version.
This probe settles it with measurement, not inference.

What it checks (measured live 2026-05-30):

  1. claude's embedded Bun build  — grep `1.3.14+<rev>` from the claude binary.
                                     RESULT: revision 521eedd6d.
  2. the stock GitHub release      — sha256 + revision of bun-v1.3.14/bun-linux-x64.
                                     RESULT: revision 0d9b296af,
                                     sha256 9fd36f87…be2ad74.
  3. ClientHello: claude vs the STOCK binary, captured fresh at a raw socket.
                                     RESULT: both 7513169ad3963133c0414ac5856f5dad.

Headline: claude embeds build 521eedd6d; the stock release is a DIFFERENT build
(0d9b296af) of the same 1.3.14 — yet both emit the identical ClientHello. So the
fingerprint is determined by the Bun MINOR VERSION (BoringSSL version + default
SSL_CTX, which Anthropic does not customize), not the exact build hash. A stock
download is therefore sufficient: AO pins the version (and the sha256 for
supply-chain integrity), matches claude's embedded `Bun/x.y.z` string, and never
needs to extract anything from the claude binary.

Get the stock binary (the path this probe defaults to):
    mkdir -p /tmp/bun1314_stock && cd /tmp/bun1314_stock
    curl -sL -o bun.zip \
      https://github.com/oven-sh/bun/releases/download/bun-v1.3.14/bun-linux-x64.zip
    unzip -o bun.zip          # -> bun-linux-x64/bun

Usage:
    python3 probe_bun_provenance.py [STOCK_BUN_PATH]
"""
import hashlib
import os
import re
import shutil
import subprocess
import sys

import ja3_diff as j

EXPECTED_FP = "7513169ad3963133c0414ac5856f5dad"
EXPECTED_STOCK_SHA = "9fd36f87e4b90b07632b987a2e4ec81ca15a62c81bf983190cea6d715be2ad74"
DEFAULT_STOCK = "/tmp/bun1314_stock/bun-linux-x64/bun"


def claude_binary():
    """Resolve the on-disk claude executable (follows the launcher symlink)."""
    path = shutil.which("claude")
    if not path:
        return None
    return os.path.realpath(path)


def grep_bun_revision(binary_path):
    """Pull the embedded `1.3.14+<rev>` build string out of a Bun-compiled binary."""
    with open(binary_path, "rb") as f:
        blob = f.read()
    rev = re.search(rb"1\.3\.\d+\+[0-9a-f]{9}", blob)
    ver = re.search(rb"Bun/[0-9]+\.[0-9]+\.[0-9]+", blob)
    return (
        rev.group().decode() if rev else None,
        ver.group().decode() if ver else None,
    )


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def bun_revision(path):
    out = subprocess.run([path, "--revision"], capture_output=True, text=True, timeout=30)
    return out.stdout.strip()


def main():
    stock = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_STOCK
    print("==== Bun provenance: stock download vs extract-from-claude ====\n")

    # 1. claude's embedded build.
    cbin = claude_binary()
    if not cbin:
        print("!! claude not on PATH — cannot read embedded Bun build.")
        return
    crev, cver = grep_bun_revision(cbin)
    print(f"[1] claude binary : {cbin}")
    print(f"    embedded Bun  : {cver}  (build {crev})\n")

    # 2. stock release identity.
    if not os.path.exists(stock):
        print(f"!! stock Bun not found at {stock}")
        print("   download it (see module docstring) then re-run.")
        return
    srev = bun_revision(stock)
    ssha = sha256(stock)
    print(f"[2] stock binary  : {stock}")
    print(f"    revision      : {srev}")
    print(f"    sha256        : {ssha}")
    print(f"    sha256 == pinned baseline: {ssha == EXPECTED_STOCK_SHA}")
    print(f"    build differs from claude's embedded build: {srev != crev}\n")

    # 3. ClientHello: claude vs the stock binary, fresh capture.
    print("[3] capturing ClientHello (claude, then stock fetch) ...", flush=True)
    cl = j.capture(j.spawn_claude)
    st = j.capture(j.spawn_js(stock))
    if not cl or not st:
        print("    !! capture failed:", "claude" if not cl else "", "stock" if not st else "")
        return
    cl_fp, st_fp = j.normalized_fp(cl), j.normalized_fp(st)
    print(f"    claude            normalized fp : {cl_fp}")
    print(f"    stock-fresh fetch normalized fp : {st_fp}")
    match = cl_fp == st_fp == EXPECTED_FP
    print(f"    MATCH (both == {EXPECTED_FP}): {match}\n")

    print("---- verdict ----")
    if match and ssha == EXPECTED_STOCK_SHA and srev != crev:
        print("STOCK download reproduces claude's ClientHello across a DIFFERENT build.")
        print("=> fingerprint is version-determined; AO pins the stock version + sha256,")
        print("   no extraction from the claude binary required.")
    else:
        print("UNEXPECTED — re-investigate before relying on a stock download:")
        print(f"   fp match={match} stock_sha_ok={ssha == EXPECTED_STOCK_SHA} "
              f"build_differs={srev != crev}")
    print("================================================================")


if __name__ == "__main__":
    main()
