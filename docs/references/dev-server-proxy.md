# Proxying a dev server

What a dev server does with the headers a reverse proxy in front of it
rewrites, and what the preview gateway
(`internal/transport/previewgateway.go`) must therefore send.

**Verified 2026-09-02 against Vite 8.2.2 with default config**, by spike
per [spike-policy.md](spike-policy.md). Everything below marked *Vite* was
observed; the Next.js note is explicitly NOT verified and is labelled so.

## Host is checked, on both paths

Vite checks the `Host` header on the HTTP path and again on the HMR
WebSocket upgrade. A rejected request gets `403 Blocked request` on HTTP
and `400` on the upgrade.

`localhost`, `*.localhost` and bare IP literals always pass. So the proxy
rewrites `Host` to `localhost:<port>` on **every** forwarded request, the
upgrade included. Rewriting only the HTTP half produces a page that loads
and an HMR socket that never connects, which is the worst of the three
outcomes: it looks like it works.

## Origin is never compared, and must still be rewritten

Vite does not compare the `Origin` VALUE. What its presence does is make
the `?token=` query parameter on the HMR upgrade mandatory.

The token is minted per server start and baked into `/@vite/client`, so
it reaches the browser through the proxy for free — the client fetches
`/@vite/client` over the same proxied origin and reads its own token out
of it. That means the token requirement is satisfied either way, so the
choice is about what else depends on the header.

**Rewrite `Origin` to `<upstream scheme>://localhost:<port>` when the
request carries one; never strip it.** The scheme is the dev server's
own, not a constant: a dev server configured with `server.https` is
reached over https, and an `Origin` naming the other scheme is a
different origin to anything that compares them. Stripping makes the token optional, which is a
weaker request than the one the browser actually sent. Rewriting also
satisfies Next.js 15+'s origin-based `allowedDevOrigins` check —
**unverified for Next.js**; stated here as the reason the rule is
"rewrite" rather than "strip", not as an observed fact.

## The upstream may be https, and the proxy must dial what it speaks

Not a Vite observation — a property of the setup. `server.https` is a
supported Vite option and other frameworks default to it, so a dev server
on loopback may be TLS. The certificate is invariably one nothing can
verify, and the hop never leaves the machine (the dial is to a loopback
literal this process chose), so the upstream transport skips verification
for the same reason the discovery probe does.

The scheme travels with the port, from the probe that found it
(`devscan.DevServer.Scheme`) through `transport.PreviewTarget` to the
dial. A port that CHANGES scheme is a different upstream and its listener
is rebuilt rather than kept.

## Path and query go through byte for byte

The upgrade is routed only when the path equals the dev server's `base`
EXACTLY. A changed path hangs the socket with no response at all — not a
refusal, not a close frame, nothing — so the proxy must preserve the
request path and query string unmodified.

`Sec-WebSocket-Protocol` is forwarded unchanged. Vite's HMR client sends
`vite-hmr`.

## `vite-ping` proves nothing

The `vite-ping` subprotocol bypasses Vite's host check entirely. A
ping-only probe through the proxy therefore succeeds whether or not the
rewrite is correct, which makes it useless as a test. The contract test
exercises a REAL upgrade and asserts the upstream saw
`Host: localhost:<port>` and the original path and query.

## Port 443 would change the client's URL, and is not our case

On a page served from port 443 the HMR client builds `wss://host:/?token=`
(an empty port segment). The preview listener mirrors the dev server's own
port number, so 443 arises only if a dev server bound 443 itself. No
action; recorded so a future reader who sees that URL shape knows where it
comes from.
