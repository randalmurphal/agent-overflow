#!/usr/bin/env bun
// tier0_sidecar.js — Tier-0 Bun outbound fingerprint sidecar (VALIDATION ONLY).
//
// The Go inbound proxy can capture + relay, but CANNOT reproduce claude's TLS+h1
// fingerprint on the OUTBOUND leg: Go's net/http canonicalizes header casing and
// Go's TLS stack presents its own ClientHello. This sidecar IS the outbound leg.
// It takes a JSON envelope describing claude's request and re-issues it with the
// version-pinned Bun `fetch`, whose ClientHello (ja3_diff.py) and h1 header block
// (probe_h1_headerforms.py) are already proven byte-identical to claude's.
//
// Envelope (POSTed by the Go proxy; claude's headers ride in the BODY so that
// Bun.serve's request-ingest lowercasing never touches their casing):
//   { url, method, headers: [[name, value], ...], body }
//
// Why rebuild the array into a PLAIN OBJECT: a plain object passed to fetch keeps
// its key casing on the wire and fetch re-sorts case-sensitively into claude's
// header order (§12). Passing the array (or a Headers) would route through
// `new Headers()` and lowercase every name. So we copy pairs into an object:
// casing preserved, order irrelevant (fetch sorts it).
//
// Streams the upstream SSE body straight back via Response(up.body) — proven
// incremental by probe_tier0_stream.py hop (b).
//
// SECURITY: never logs Authorization / cookie / token VALUES. The optional
// TIER0_SIDECAR_LOG file records only the header NAMES handed to fetch (for the
// Q2(ii) casing-survival cross-check) plus whether the response was compressed.
import { appendFileSync } from "fs";

const PORT = Number(process.env.SIDE_PORT || 0);
const LOG = process.env.TIER0_SIDECAR_LOG || "";

// Response headers that must NOT pass through to claude: Bun fetch has already
// auto-decompressed the body, so content-encoding/content-length are now stale
// lies; transfer-encoding/connection are framing Bun.serve regenerates itself.
const STRIP_RESP = new Set([
  "content-encoding",
  "content-length",
  "transfer-encoding",
  "connection",
]);

function logLine(obj) {
  if (!LOG) return;
  try {
    appendFileSync(LOG, JSON.stringify(obj) + "\n");
  } catch (_) {
    // logging must never break the relay
  }
}

const server = Bun.serve({
  port: PORT,
  hostname: "127.0.0.1",
  // A turn's first token can lag many seconds behind the envelope POST (the model
  // thinks before emitting). idleTimeout measures the Go<->sidecar connection's
  // idle gap; keep it at Bun's max so a slow first token never severs the relay.
  idleTimeout: 255,
  async fetch(req) {
    if (req.method !== "POST") {
      return new Response("tier0 sidecar: POST a request envelope", { status: 405 });
    }

    let envelope;
    try {
      envelope = await req.json();
    } catch (_) {
      return new Response(JSON.stringify({ error: "bad envelope" }), { status: 400 });
    }

    const { url, method, headers, body } = envelope;

    // Rebuild plain-object headers, preserving claude's original casing.
    const hdr = {};
    const names = [];
    for (const [name, value] of headers || []) {
      hdr[name] = value;
      names.push(name);
    }

    const noBody = method === "GET" || method === "HEAD";
    let up;
    try {
      up = await fetch(url, {
        method: method || "POST",
        headers: hdr,
        body: noBody ? undefined : body,
      });
    } catch (e) {
      logLine({ handed_to_fetch: names, error: "upstream fetch failed" });
      return new Response(JSON.stringify({ error: "upstream fetch failed" }), {
        status: 502,
      });
    }

    // Observation, not action: record whether the origin compressed (all prior
    // captures stripped Accept-Encoding, so this is the first real look). After
    // auto-decompression Bun usually drops content-encoding from up.headers; if it
    // lingers, STRIP_RESP removes it so claude never double-decodes.
    logLine({
      handed_to_fetch: names,
      resp_status: up.status,
      resp_content_encoding: up.headers.get("content-encoding") || "(none)",
      resp_content_type: up.headers.get("content-type") || "(none)",
    });

    const respHeaders = {};
    for (const [name, value] of up.headers) {
      if (!STRIP_RESP.has(name.toLowerCase())) respHeaders[name] = value;
    }

    // Response(up.body, ...) streams the upstream ReadableStream straight through.
    return new Response(up.body, { status: up.status, headers: respHeaders });
  },
});

console.log(JSON.stringify({ port: server.port }));
