# Remote Access: Boundaries and Guarantees

Companion to [remote-access.md](./remote-access.md). Produced by a
robustness review of that spec, 2026-08-04.

This doc states what the access model guarantees, what it deliberately
does not, and the specific cases the design has to handle. It is the
reference for reviewing changes that touch listeners, routes, event
channels, or credentials.

## Confirmed defects (2026-08-04; A and B FIXED 2026-08-18)

Found by audit, each step verified in code. All three were reachable
**today in the desktop webview**, with no remote feature enabled. Fixes
are spec §16 phase 0.

> **Status 2026-08-18: A and B are fixed at their shared root cause.**
> `Link.svelte` no longer has the `isPathRelativeUrl` raw-anchor
> branch, and `Image.svelte` no longer has its raw-`src` twin
> (`frontend/src/lib/markdown/AGENTS.md` § Security boundary): an anchor or `<img>` renders only for
> a `transformUrl`-approved URL, so neither a `/`-leading nor a
> `//host` href can top-level-navigate the window (or issue a raw
> same-origin fetch) from model output. Path-shaped markdown hrefs
> are instead rewritten during parsing to the nonce'd
> `agent-overflow:open` editor scheme (`pathLinkExtension.ts`), only
> on surfaces that carry a workspace, so third-party PR/review text
> never grows editor affordances. The click-time gate is
> `editor.ResolvePath`: existing regular files open from anywhere (a
> deliberate 2026-08-18 loosening), folder opens are refused
> everywhere (`.vscode/` tasks execute on folder open), UNC paths and
> out-of-workspace scaffolding stay refused. The bodies of A and B
> below are kept in their original present-tense form as the record
> of what was reachable. **C remains open**, as do the `/design/`
> hardening items (CSP/nosniff/XFO/token) that defense-in-depth still
> wants even with the navigation path closed.
>
> **Re-verified 2026-08-30** against the first-party markdown
> renderer (svelte-streamdown was adopted into `src/` since). Both
> render paths — the component path and the compact fixed-tag HTML
> path, a pair `markdown/AGENTS.md` flags as a silent-fork hazard —
> agree and call the same gate. The gate now fails closed
> structurally: `parseUrl` is `new URL()` with the upstream base
> parameter deleted, so `/design/x`, `//host/x`, and `*` all throw
> rather than resolve. `//`-leading is explicitly excluded from the
> schemeless class in both paths, so it renders as a tagged blocked
> span. What did **not** change: the click delegate's
> no-`preventDefault` fall-through is still there (now unreachable
> from markdown, still app-wide policy for every other anchor), the
> bootstrap token is still in `sessionStorage` and on
> `window.__AO_BOOTSTRAP__`, and `/design/` is untouched — its last
> functional commit predates the audit. Two unvalidated hrefs in
> `PRStep.svelte` remain, and are now outliers: both sibling
> consumers of the same field validate.

**A. Model-authored content can reach the full method surface.**
The markdown renderer's `Link.svelte`
(`frontend/src/lib/markdown/render/elements/`) renders any href where
`isPathRelativeUrl` is true (literally `startsWith('/')`) through a
branch that **bypasses `transformUrl`**, emitting the raw href with no
`target`/`rel`. The click delegate's `safeExternalURL` returns null for
anything that is not `http(s)` and then returns *without*
`preventDefault`, so the browser performs a same-tab top-level
navigation. `/design/` serves agent-written files from the SPA origin as
`text/html` with no CSP, no `nosniff`, no `X-Frame-Options`, and no
token. Script running there reads
`sessionStorage['ao:bootstrap-token']`, opens `/ws?token=…` as a
loopback peer (so `LocalOnlyMethods` does not apply), and reaches every
bound method. A design-mode agent writes the file itself as ordinary
output; the link text is model-authored.

**B. The app window can be replaced, with no design mode involved.** The
same branch treats a protocol-relative `//other-host.example` as
"relative", so model output alone can navigate the top-level window
off-origin. The credential is not readable across origins, but the app
window becomes third-party content with no route back, a convincing
surface for a misleading prompt.

**C. Unauthenticated local file read.** `http.Dir` blocks `../` but does
not resolve symlinks, and `/design/` requires no token at all. A symlink
created in an agent workdir exposes any user-readable file. There is
also no per-thread scoping, and directory listings are enabled.

The preview iframes are correctly sandboxed (`allow-scripts` without
`allow-same-origin`). A and B route *around* that sandbox via top-level
navigation rather than through it.

The rest of the render pipeline audited clean and should not be
re-litigated: raw HTML disabled (`renderHtml={false}`), non-`http(s)`
URL schemes rejected for links and images, path-link prefix carries a
128-bit per-page-load nonce so it cannot be forged from model text,
KaTeX runs with `trust: false`, mermaid runs `securityLevel: 'strict'`
and sanitizes labels with the DOMPurify copy it bundles (we no longer
depend on DOMPurify directly), terminal rendering uses the real xterm parser,
the non-xterm ANSI renderer escapes all five characters and builds class
names from parsed integers, highlight spans are metadata rendered
through the template, no `eval`/`new Function` anywhere, and no
credential in localStorage.

Root cause of A and B is a single branch in a file the repo already
patches.

## Also confirmed (2026-08-30)

Found by a follow-up audit of the design route, the in-app browser,
and the full listener set. Same posture as above: verified in code,
reachable today, fixes are spec §16 phase 0.

**D. The in-app browser widened who counts as a loopback peer.**
`Manager.Open` accepts loopback URLs — correct for the dev servers it
exists to show, but it also lets a page reach the app's own transport
port and the auxiliary listeners. Two consequences: an agent can read
any thread's `/design/` workdir through `browser_open` +
`browser_dom`, around the workspace containment `browser_open_file`
enforces (two tools, two containment models, one routes past the
other); and arbitrary web content becomes a loopback peer that did
not exist a month ago. The pixels-only companion pane is *not* the
weak point — screencast JPEGs into an `<img>`, no DOM crossing, an
ephemeral Chrome profile with no access to the webview's
`sessionStorage`. That part of the design is right and holds.

**E. The `--connect` client stub serves the upstream credential.**
Its loopback listener returns the injected `__AO_BOOTSTRAP__` — token
included — on `GET /`, behind a Host guard and nothing else. Any
loopback peer on the machine running `--connect` reads a working
credential for the remote backend.

**F. Both MCP endpoints authenticate on the path alone.** The browser
and design MCP servers check only the method and an unguessable
per-thread UUID in the path: no `Origin` or `Sec-Fetch-Site`
rejection, no loopback-peer assertion, and the body is decoded
regardless of `Content-Type`. The claudetui gateway next door already
does the peer check. The grant behind that path is page evaluation
and workspace file reads, so this is a larger capability than "an
auxiliary listener with no session credential" implies (spec §13's
rule is reworded accordingly). The path also rides the provider CLI's
argv, readable from `/proc/<pid>/cmdline` by any process of the same
user — same-user is already the trust boundary, but it means the
credential is not secret from local software the way an in-memory
token is.

**G. The two Chrome launchers disagree on sandbox posture.**
`internal/screenshot` disables the OS sandbox while rendering
agent-authored HTML; `internal/browser` explicitly refuses the same
flag and comments that failing to launch is the safer outcome. Same
class of content, opposite decision, same codebase.

**Not a defect, checked and cleared**: the open CORS header on the
embedded `modern-screenshot` asset is deliberate and required — the
capture iframe fetches it from an opaque sandbox origin, which cannot
be named in an allow-list. Its only side effect is that the asset is
cross-origin readable, which reveals the port to a page that probes
for it; the port is already discoverable to anything that can probe
loopback, so this changes nothing. It stays, declared in the
enumeration as a static asset carrying no data. Likewise the browser
MCP listener cannot start lazily: its URL rides provider argv at
spawn, so it must exist before the first tool call.

## What a session credential represents

A session carrying execute-tier scopes can start provider processes, run
PTYs, and execute git on the owner's machine, and a thread in
`full-access` mode runs tool calls without prompting. So an
execute-tier credential is equivalent to a shell on that machine. Every
decision in the spec follows from treating it that way.

## What the model guarantees

- Reaching the RPC surface requires a credential the owner issued.
- A credential that leaks does not become permanent access: refresh
  rotates, reuse is detected, and revocation reaches live connections.
- A credential cannot be presented more weakly than it was issued.
  Binding travels with the credential, not the socket, so a softer
  listener cannot be used to downgrade it.
- A single leaked artifact is never sufficient on its own: pairing links
  need the device key, tickets are single-use and key-bound, and the
  step-up set needs a fresh proof.
- Every privileged action is attributable to a device.

## What the model does not guarantee

The session rows and the signing key live on the same machine, under the
same user, as the processes a session can start. Once something reaches
execute-tier capability locally, it can mint its own credentials and
rewrite any on-machine record. **The access model's value is entirely in
gating what happens before that point, plus off-machine
accountability.**

Two consequences: do not spend design effort on controls that an
execute-tier holder can simply bypass, and treat the audit record as
useful mainly (a) off the machine and (b) as tamper-evident rather than
tamper-proof.

## Cases the design must handle

| # | Case | How the design handles it |
|---|---|---|
| 1 | A pairing link is seen by someone else: pasted into a chat, photographed, or read by a page that renders the fragment | The redeeming device's key thumbprint is part of the exchange, plus an owner-confirmed verification number. The link alone does nothing. |
| 2 | Someone else redeems the link first | The verification number must match the device the owner is looking at, so a silent redemption fails confirmation. |
| 3 | A token is copied out of a browser or a phone's storage | Execute tier requires `binding ≥ device-bound`; refresh is key-bound and rotating, so the copy either cannot renew or trips reuse detection and revokes the family. |
| 4 | A key-bound token is presented as a plain bearer on a softer listener | Binding is a session attribute enforced on every listener, including loopback. |
| 5 | A ticket leaks from a URL via proxy logs, an intermediary, or browser internals | Tickets are single-use, consumed at upgrade, and redemption requires a key proof; connections re-validate liveness and cap their lifetime. |
| 6 | The owner revokes a device that currently holds a live WebSocket | Revocation force-closes matching connections and invalidates the in-memory session synchronously; no RPC authorizes from state cached at upgrade. |
| 7 | A session is used to re-point provider traffic (`ANTHROPIC_BASE_URL`) or to register a stdio MCP server that runs a chosen binary | Both are in the step-up set: fresh proof per call, never an ambient scope. |
| 8 | A session is used to install a replacement binary via self-update | Download/apply are `scope: host`; remote trigger needs step-up plus artifact signature verification, behind a rollback watchdog. |
| 9 | Repeated guessing against `/auth/token` or the ticket endpoint from behind a tunnel, where every request shares one source address | Limits keyed by token/account with a global counter across listeners; real client address taken from our own validated forwarded header, never the tunnel's. |
| 10 | A web page uses DNS rebinding to address the loopback listener | Strict Host allow-list, Origin / `Sec-Fetch-Site` checks on `/ws` and auth endpoints, rebound Hosts rejected. |
| 11 | Something on the Windows side reaches the WSL launcher relay port and inherits webview trust | The relay forwards the webview's actual `loopback-only` credential; apparent loopback origin stops being a trust basis. Relay listener binds `127.0.0.1`. |
| 12 | A peer backend performs bulk retrieval across every enrolled thread | Sensitive content classes withheld or redacted by default with explicit opt-in; peer reads rate-limited and audited per peer; enrollment documented as one-way disclosure. |
| 13 | A client claims scopes it was not granted | The client capability object is UI-only; the server re-checks every RPC against the authenticated session. |
| 14 | On-machine records are altered to hide activity | Audit is an `O_APPEND` hash-chained file with no wire mutation path, mirrored off-machine. Evident, not prevented. See the section above. |
| 15 | A revoked or lost device still holds its synced replica | Revocation cuts access, not past disclosure. The phone replica is encrypted at rest with a key in native secure storage; browser replicas are not, and whatever a device already synced must be assumed readable to whoever controls that device. |
| 16 | A backend under someone else's control serves a modified phone bundle | The shell verifies every bundle against the release signing key baked into the shell; backends can only relay genuine signed releases. One such backend cannot reach the phone's device keys or its other backends' credentials through an update. Dev-bundle trust is an explicit per-device opt-in. |
| 17 | A page loaded in the in-app browser addresses the app's own loopback ports | Managed-browser navigation denies our own transport and auxiliary ports (dev-server ports are unaffected); `/design/` carries a per-thread token, so reaching the port is not reaching the content. |
| 18 | A local process reads an auxiliary listener's path credential out of provider argv | The listener re-checks that the peer is loopback and refuses browser-originated requests, so holding the URL is not sufficient by itself. Same-user local software remains inside the trust boundary by construction (see the section above). |

## Claims we deliberately do not make

- **Non-extractable WebCrypto keys do not contain script running in the
  page.** In-page script can call the signing API or simply issue RPCs
  on the live session while the tab is open. Non-extractability bounds
  *reuse after the page closes*, nothing more. The real mitigations are
  CSP, not having injectable content, and keeping execute-tier scopes
  off browser sessions where the owner chooses to narrow them.
- **Fragment placement of pairing tokens** prevents logging and Referer
  leakage. It is not what makes pairing safe. The device key is.
- **The 404 / low-fingerprint posture** is cheap defense-in-depth, not a
  control; timing and behavioral characteristics still identify the
  service.
- **Rate limits** bound repeated guessing only. They do nothing about a
  link that was seen by someone else, which is the realistic pairing
  concern.

## Review posture for future changes

- Adding a bound method: the generator forces a scope declaration.
  Choosing an execute-tier scope means accepting that a leaked
  credential of that class can perform the action remotely. Say so in
  the commit message.
- Adding a listener: declare its binding class and what it accepts. A
  listener that accepts weaker presentations than an existing one
  reintroduces the downgrade closed in case 4.
- Adding a route or event channel: declare tier, scope, and content-type
  posture. Unclassified means unbuilt (spec §13).
- Adding a settings key: pick its tier. Host tier reconfigures the
  backend and needs step-up.
- Widening a peer or viewer scope: re-read the disclosure note in spec
  §11 first. Sharing is one-way and cannot be undone.
