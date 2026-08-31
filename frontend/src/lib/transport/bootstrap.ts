// Bootstrap manifest handling for the transport: the /bootstrap.json
// fetch that exchanges the page's one-time ticket for its session
// cookie, and the WS-URL validation that keeps a tampered manifest from
// pivoting the connection to another origin or scheme.
//
// The page holds no credential of its own. It arrives carrying a
// one-time ticket in `?t=`, spends it on the first manifest fetch, and
// from then on the server's HttpOnly cookie authenticates every request
// this document makes — the manifest refetch, the WebSocket upgrade, a
// reload. Nothing readable from script is involved, which is why there
// is no token field on the manifest and no stash anywhere in this file.

import { setViewOnlySessionFromBootstrap } from './runMode';
import { setHarnessPageMarkerFromBootstrap, setHarnessSessionFromBootstrap } from './harnessMode';
import { setBackendIdentityFromBootstrap } from './backendIdentity';
import { clampString } from './frames';

// BootstrapRejectedError marks the one bootstrap failure that retrying
// cannot fix: the server answered, and refused our credential. The
// cookie is minted per backend launch (internal/transport/server.go
// handleBootstrap answers an unrecognised credential with 404,
// deliberately indistinguishable from "no such path"), so a remote/LAN
// client whose backend restarted holds a cookie that will never be
// honoured again — only reopening the share link mints a new one.
// Distinct from a transient failure (network error, the 503 readiness
// gate, the 500 startup-failure page) so the transport can surface an
// actionable state instead of a silent forever-loop.
export class BootstrapRejectedError extends Error {
  status: number;
  constructor(status: number) {
    super(`bootstrap credential refused: HTTP ${status}`);
    this.name = 'BootstrapRejectedError';
    this.status = status;
  }
}

// HTTP statuses that mean "your credential is not valid here" rather
// than "try again later".
const CREDENTIAL_REFUSED_STATUSES = new Set([401, 403, 404]);

// The URL parameter carrying the one-time page ticket. Mirrors
// transport.PageTicketParam (internal/transport/credential.go).
const PAGE_TICKET_PARAM = 't';

// isLoopbackHostname reports whether a document host names this machine.
// Exported pure so the predicate is testable without a document.
//
// The Go counterpart is internal/loopback (which names this function in
// its package doc). This one stays here — a different language with no
// way to call that one — and it is deliberately WIDER than
// loopback.EndpointHostname: it also accepts any *.localhost name,
// because a document host is the browser's own idea of where it is
// rather than something this process is deciding to trust. Nothing here
// authorizes anything; the decision below is only whether a retry is
// worth attempting.
//
// The distinction matters exactly once: a refused credential is only
// terminal for a session that cannot mint a new one. The embedded
// webview and the `--connect` stub always load from loopback and are
// handed a fresh ticket by the shell that owns the backend, so a refusal
// there is a transient boot race worth retrying. A page served over the
// network got its ticket from a share link, and only re-opening that
// link (a fresh page load) can produce another one.
export function isLoopbackHostname(hostname: string): boolean {
  const host = hostname.trim().toLowerCase();
  if (host === 'localhost' || host.endsWith('.localhost')) return true;
  // location.hostname strips the brackets from an IPv6 authority, but
  // accept the bracketed form too so a hand-built host string matches.
  if (host === '::1' || host === '[::1]') return true;
  // IPv4 loopback is the whole 127.0.0.0/8 block, not just 127.0.0.1.
  return /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(host);
}

// pageServedOverLoopback answers isLoopbackHostname for the current
// document. A non-browser embedding (no window/location) answers "yes":
// the remote-only terminal state must never be reachable from a context
// whose locality we cannot establish.
export function pageServedOverLoopback(): boolean {
  if (typeof window === 'undefined' || typeof window.location === 'undefined') return true;
  return isLoopbackHostname(window.location.hostname ?? '');
}

// Bootstrap is the JSON the SPA fetches at /bootstrap.json on first
// load. Mirror the Go-side shape (internal/transport/server.go
// Bootstrap, and internal/clientmode's manifestJSON, which answers the
// same shape from the `--connect` stub's own origin).
//
// It carries no credential. Everything here is a fact about the backend
// the page just authenticated to.
export interface Bootstrap {
  wsUrl: string;
  /**
   * Identifies this backend launch. Not a credential and not a secret:
   * it exists so per-tab state that must not survive a backend restart
   * (the notification replay checkpoint in wsClient.ts) can tell one
   * launch from the next.
   */
  launchId?: string;
  remote?: boolean;
  /**
   * Stable per-store UUID keying the client-side thread replica, plus
   * the generation that is re-minted whenever the backend's history
   * counters lose continuity (docs/architecture/thread-replica-sync.md §3.3).
   * Absent from the `--connect` stub's manifest — absence disables the
   * replica rather than sharing another backend's database.
   */
  backendId?: string;
  replicaGeneration?: string;
  /**
   * The backend was booted as the agent test harness or the soak rig
   * (`--harness` / `--soak`), which is the only thing that arms the
   * frontend harness bridge. Absent on every ordinary boot; see
   * ./harnessMode.ts.
   */
  harness?: boolean;
  pageMarker?: string;
}

// defaultBootstrap fetches /bootstrap.json from this page's own origin,
// spending the one-time `?t=` ticket if the URL still carries one. This
// runs the first time anyone calls `ensureConnected`; subsequent calls
// reuse the cached promise, and the reconnect path refetches through the
// same function.
//
// Every boot flow lands here — embedded webview, `--connect` stub, WSL
// launcher window, LAN browser, dev (where the Go server proxies Vite
// and the page origin is still the Go server's). They differ only in who
// served the page; the credential exchange and the same-origin manifest
// are identical, which is what lets this file hold no per-flow branches.
export async function defaultBootstrap(): Promise<Bootstrap> {
  const search = typeof window !== 'undefined' ? window.location.search : '';
  const ticket = new URLSearchParams(search).get(PAGE_TICKET_PARAM) ?? '';
  return fetchManifest(ticket);
}

// fetchManifest is the one /bootstrap.json fetch + validation path.
//
// The ticket rides the request when the URL still carries one; on every
// later fetch there is none and the cookie speaks alone. The server
// treats a live cookie as sufficient (internal/transport/credential.go
// Exchange authenticates first, and only then looks at the ticket), so
// re-presenting a spent ticket is harmless and a refetch mid-session
// needs nothing from the URL.
async function fetchManifest(ticket: string): Promise<Bootstrap> {
  const url = ticket === ''
    ? '/bootstrap.json'
    : `/bootstrap.json?${PAGE_TICKET_PARAM}=${encodeURIComponent(ticket)}`;
  // same-origin credentials is the default for a same-origin request,
  // but state it: this fetch is the cookie's whole delivery path, and a
  // future caller passing a different mode would silently unauthenticate
  // the page.
  const resp = await fetch(url, { credentials: 'same-origin' });
  if (!resp.ok) {
    if (!CREDENTIAL_REFUSED_STATUSES.has(resp.status)) {
      // Transient: the server is up but not serving the manifest yet
      // (503 readiness gate, 500 startup failure) or something in
      // between failed. The cookie the exchange already set is still
      // the right one — the server issues it before those gates run.
      throw new Error(`bootstrap fetch failed: HTTP ${resp.status}`);
    }
    throw new BootstrapRejectedError(resp.status);
  }
  const contentType = resp.headers.get('content-type') ?? '';
  if (!contentType.toLowerCase().startsWith('application/json')) {
    throw new Error(`bootstrap response not JSON: content-type ${clampString(contentType)}`);
  }
  const data = (await resp.json()) as Bootstrap;
  if (!data || typeof data !== 'object') {
    throw new Error('bootstrap response not an object');
  }
  if (typeof data.wsUrl !== 'string') {
    throw new Error('bootstrap response missing wsUrl');
  }
  validateWsUrl(data.wsUrl);
  data.remote = data.remote === true;
  setViewOnlySessionFromBootstrap(data.remote);
  setHarnessPageMarkerFromBootstrap(data.pageMarker);
  // The harness bridge registers its page synchronously when this latch
  // flips. Publish the marker first so that first-load registration cannot
  // race with the waiter callback.
  setHarnessSessionFromBootstrap(data.harness === true);
  // Re-validated on every manifest resolution, which is what makes a
  // mid-session generation re-mint (a restored backend) observable on
  // the reconnect refetch rather than at the next app launch.
  setBackendIdentityFromBootstrap(data.backendId, data.replicaGeneration);
  // Remove the spent ticket from history, Referer, and Performance
  // Resource Timing entries. The cookie carries the session from here,
  // so a reload of the scrubbed URL still boots. Skip when
  // history.replaceState isn't available (older happy-dom builds, weird
  // host pages).
  if (
    ticket !== '' &&
    typeof window !== 'undefined' &&
    typeof window.history !== 'undefined' &&
    typeof window.history.replaceState === 'function'
  ) {
    try {
      const retained = new URLSearchParams(window.location.search);
      retained.delete(PAGE_TICKET_PARAM);
      const suffix = retained.toString();
      window.history.replaceState(null, '', window.location.pathname + (suffix ? `?${suffix}` : '') + window.location.hash);
    } catch {
      // Some embeddings throw on replaceState; the ticket is spent
      // either way, so swallowing is acceptable.
    }
  }
  return data;
}

// wsUrlMatchesPageOrigin reports whether wsUrl addresses the same origin
// the page was served from. Pure and exported so the comparison is
// testable against origins this document will never have — the same
// split as isLoopbackHostname / pageServedOverLoopback above.
//
// Origin here is scheme + host + PORT, with ws:/wss: mapped onto their
// http:/https: counterparts. Host alone would not do: a second listener
// on the same machine is a different principal, and on a LAN bind it
// need not be ours at all. Host alone is also exactly what a cookie
// scopes to, which is why this check and the server's own Origin check
// on the upgrade are both port-aware. The scheme pairing is what stops a
// TLS-fronted page being moved onto a cleartext socket. Explicit default
// ports normalise away on both sides (ws:/http: share 80, wss:/https:
// share 443), so the two spellings still match.
export function wsUrlMatchesPageOrigin(
  wsUrl: string,
  page: { protocol: string; host: string },
): boolean {
  let parsed: URL;
  try {
    parsed = new URL(wsUrl);
  } catch {
    return false;
  }
  const wantProtocol = parsed.protocol === 'wss:' ? 'https:' : 'http:';
  if (page.protocol.toLowerCase() !== wantProtocol) return false;
  // location.host and URL.host are already lowercase-normalised by the
  // URL parser; lowercase anyway so a hand-built page object matches.
  return parsed.host.toLowerCase() === page.host.toLowerCase();
}

// validateWsUrl rejects a bootstrap manifest that points the client's
// WebSocket somewhere it must not go. Two checks, both unconditional:
//
//  1. Scheme. A manifest can't pivot the connection to an arbitrary URL
//     handler.
//  2. Origin. Every manifest the SPA can receive is served by the same
//     origin as the page, and names a wsUrl that server derived from
//     this very request's Host header (internal/transport/server.go
//     deriveWSURL, and clientmode's manifestJSON via the same helper).
//     Anything naming another authority was tampered with in flight.
//
// There is no exemption and no caller-supplied opt-out. The `--connect`
// stub used to be one: it injected a manifest naming a remote backend
// and the page opened a cross-origin socket. It now serves its own
// origin's manifest and carries the socket to the upstream itself, so
// the page's rule is the same in every mode — and a rule with no
// parameter cannot be passed the wrong argument.
export function validateWsUrl(wsUrl: string): void {
  let parsed: URL;
  try {
    parsed = new URL(wsUrl);
  } catch {
    throw new Error(`bootstrap wsUrl invalid: ${clampString(wsUrl)}`);
  }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    throw new Error(`bootstrap wsUrl scheme not ws/wss: ${clampString(parsed.protocol)}`);
  }
  if (typeof window === 'undefined' || typeof window.location === 'undefined') {
    // An origin requirement we cannot evaluate is a requirement we
    // cannot meet. Unreachable in practice — the only caller fetched a
    // relative '/bootstrap.json', which already needs a document base —
    // but it must fail closed, not open.
    throw new Error('bootstrap wsUrl cannot be origin-checked: no document origin');
  }
  if (!wsUrlMatchesPageOrigin(wsUrl, window.location)) {
    throw new Error(`bootstrap wsUrl not same-origin: ${clampString(wsUrl)}`);
  }
}
