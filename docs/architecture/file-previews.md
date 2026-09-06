# Generated HTML previews

Click an HTML file link in a conversation to open the page on the computer that
owns that conversation. The desktop and compact layouts use the same path-link
parser and click handler. Source files keep their editor behavior on the host;
on a remote frontend, only HTML paths become actionable. A host advertises
`preview.files.v1`; older hosts keep the existing link behavior until updated.

The file is read when requested. Relative scripts, stylesheets, images and
root-relative assets resolve under the thread's workspace. An explicitly linked
file outside that workspace gets its containing folder as its root. No build or
dev server is needed for a standalone page. A framework app that needs a server
continues using the existing dev-server preview gateway.

## Serving and authorization

`MintFilePreviewURL` requires `preview:open`, and the frontend pins the call to
the link's computer before awaiting it. It refuses missing/offline computers and
discards a response if that connection was forgotten and replaced during the
call. Filesystem paths are never used to infer a different owner.

The manager in `internal/filepreview` gives each directory its own transport
preview gateway at a separate port. It shares the existing one-minute single-use
ticket exchange, twelve-hour browser grants, per-request session revocation and
held-connection checks. App credentials never appear in the URL or document.
The page runs outside the app origin and cannot access app localStorage or its
native bridge. HttpOnly app cookies sharing the hostname remain protected by the
app's exact-origin gates; origin isolation alone does not isolate cookies.

On-host callers use a literal `127.0.0.1` HTTP listener and need no networking
setup. Host presence comes from transport's kernel-derived caller proof; it is
not a user-supplied option. Remote callers use the same TLS sources as dev-server
previews: tailnet HTTPS first, LAN second. A private LAN certificate pinned by
the Android app is **not automatically trusted by an external browser**. Use
tailnet HTTPS or install appropriate browser trust for private LAN previews;
the app does not disable browser certificate verification.

The filesystem handler uses `os.Root` for race-resistant containment, refuses
hidden paths and non-regular files, never lists directories, and serves only
GET/HEAD. Responses stream through `http.ServeContent` with range support,
`no-store`, `nosniff`, and no referrer. Generated code may execute at this preview
origin, but service-worker script requests are refused so workers cannot persist
across directory/port reuse. This is a page preview, not a persistent PWA host.

## Lifetime

At most sixteen directory gateways remain open per backend; opening another
retires the least recently opened. Reopen an old link to create a fresh preview.
Each new gateway has a fresh grant book, so a cookie for a retired directory
cannot authorize a new one even when the same port is allocated again.

Disconnecting the app does not close browser previews. Revoking the device ends
its access; changing LAN/tailnet sharing closes network previews; local pages
remain usable. Restarting the backend closes all previews, and opening the link
again creates a new one. The manager owns no transcripts or provider state.

## Validation

`internal/filepreview` covers containment, hidden files, non-regular files,
streaming ranges, URL escaping, unauthenticated requests, remote cleartext
refusal, shutdown and directory bounds. App tests exercise authenticated locality
and sharing-policy retirement. The desktop/compact file-preview browser flow
opens generated HTML through real pairing, executes scripts and CSS, verifies
reload and service-worker refusal, changes sharing and revokes the device.
