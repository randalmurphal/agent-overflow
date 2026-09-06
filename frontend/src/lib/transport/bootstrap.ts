import { networkFetch } from './networkFetch';
// Bootstrap manifest handling for the transport: the /bootstrap.json
// fetch that exchanges the page's one-time ticket for its session
// cookie, and the WS-URL validation that keeps a tampered manifest from
// pivoting the connection to another origin or scheme.
//
// The page holds no credential of its own. It arrives carrying a
// one-time ticket, spends it on the first manifest fetch, and from then
// on the server's HttpOnly cookie authenticates every request this
// document makes — the manifest refetch, the WebSocket upgrade, a
// reload. Nothing readable from script is involved, which is why there
// is no token field on the manifest and no stash anywhere in this file.
//
// The ticket arrives by one of two channels, decided by who served the
// document rather than by anything this file branches on twice. A
// BROWSER can only be told things through its URL, so its ticket rides
// `?t=` and is scrubbed once spent. A page hosted by a WINDOW this
// application owns is handed its ticket by injection instead, and its
// URL carries no credential at all (./pageHost.ts). Both end at the same
// exchange, on the same route, with the same cookie.

import { setPageGrantsFromBootstrap, setCarriedSessionScopes } from './scopes';
import {
  publishManifestBackends,
  readBackendDescriptors,
  setBackendManifestFetcher,
} from './manifestBackends';
import { setHarnessPageMarkerFromBootstrap, setHarnessSessionFromBootstrap } from './harnessMode';
import { setPasskeysAvailableFromBootstrap } from './passkey';
import { setBackendIdentityFromBootstrap } from './backendIdentity';
import { clampString } from './frames';
import { fetchPairedComputer, hasPairedSession, observePairedComputerBootstrap, pairedSessionHeaders, renewPairedSession } from './deviceSession';
import { computerResponseURL } from './computerRoutes';
import type { ComputerRoute } from './computerRoute';
import { awaitInjectedPageTicket, clearInjectedPageTicket, isWebviewHosted } from './pageHost';
import { homeCredentials, homeOriginParts, homeUrl, originPartsOf } from './homeEndpoint';
import { HOME_BACKEND, type BackendKey } from './backendKey';
import { isNativeShell } from '../native/platform';
import { hasHomeEndpoint } from './homeEndpoint';

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
  constructor(status: number, readonly paired = false) {
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
export interface AttachedBackend {
  /** This backend's own address for the profile, and the id in every URL below. */
  id: string;
  /** What that machine calls itself. The identity a replica is keyed by. */
  backendId?: string;
  /** What to show a person: the owner's nickname, else the machine's name. */
  name?: string;
  /** Explicit profile override; empty means use the live host name. */
  nickname?: string;
  /** Absolute, same-origin. A WebSocket needs a scheme. */
  wsUrl: string;
  /** A same-origin path, which is what a fetch takes. */
  bootstrapUrl: string;
}

export interface Bootstrap {
  routes?: ComputerRoute[];
  sessionScopes?: string[];
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
   * The backend's display name — its hostname, the same string the hello
   * frame carries and the pairing payload showed. Display only: nothing
   * is keyed on it and `backendId` stays the identity. Absent means the
   * backend published none.
   */
  backendName?: string;
  /**
   * Every OTHER machine this installation has attached, each with the
   * same-origin URLs this backend carries it on
   * (internal/transport/attachedroutes.go). Absent when there are none,
   * which reads as "this page talks to one backend" — the shape every
   * client had before attaching existed.
   *
   * Reachability is deliberately absent: nothing is probed to answer a
   * page load, and each socket is the only current answer.
   */
  backends?: AttachedBackend[];
  /**
   * The backend was booted as the agent test harness or the soak rig
   * (`--harness` / `--soak`), which is the only thing that arms the
   * frontend harness bridge. Absent on every ordinary boot; see
   * ./harnessMode.ts.
   */
  harness?: boolean;
  pageMarker?: string;
  /**
   * The backend has a canonical domain, so it can run passkey ceremonies
   * (internal/app/app_passkey.go — WebAuthn requires a domain and refuses
   * an address, so a backend without one has no relying party to be).
   *
   * Manifest rather than hello, because the page that most needs it is the
   * one whose socket this backend will not open: a browser that has never
   * paired sees no hello at all, and a passkey is exactly how it gets in.
   */
  passkeysAvailable?: boolean;
}

// defaultBootstrap fetches /bootstrap.json from this page's own origin,
// spending the one-time ticket its host gave it — off the URL for a
// browser, by injection for a page whose window this application owns.
// This runs the first time anyone calls `ensureConnected`; subsequent
// calls reuse the cached promise, and the reconnect path refetches
// through the same function.
//
// Every boot flow lands here — embedded webview, `--connect` stub, WSL
// launcher window, LAN browser, dev (where the Go server proxies Vite
// and the page origin is still the Go server's). They differ only in who
// served the page; the credential exchange and the same-origin manifest
// are identical, which is what lets this file hold no per-flow branches.
export async function defaultBootstrap(): Promise<Bootstrap> {
  // A new phone has no legacy home connection. Never send its bootstrap
  // to the APK's asset origin; its saved computers bootstrap independently.
  if (isNativeShell() && !hasHomeEndpoint()) throw new BootstrapRejectedError(401, true);
  if (isWebviewHosted()) {
    // The host window injects the ticket; there is none on the URL to
    // read and nothing to scrub afterwards. A delivery that never
    // arrives rejects rather than hanging, and rejects TRANSIENTLY —
    // the reconnect ladder's next attempt re-announces this document to
    // its host, which answers with a fresh ticket.
    //
    // A refetch re-presents the ticket already delivered, which the
    // server treats as it treats a spent one on a reloaded URL: the
    // cookie authenticates first. Only a REFUSAL means that ticket is
    // worth nothing, and then the stale copy has to go or the retry
    // would present it forever.
    try {
      return await fetchManifest(await awaitInjectedPageTicket());
    } catch (err) {
      if (err instanceof BootstrapRejectedError) clearInjectedPageTicket();
      throw err;
    }
  }
  const search = typeof window !== 'undefined' ? window.location.search : '';
  const ticket = new URLSearchParams(search).get(PAGE_TICKET_PARAM) ?? '';
  return fetchManifest(ticket);
}

// Home and attached backends share credential presentation and recovery.
// Snapshot pairing before header generation: a missing device key can clear
// storage, but the remedy is still pairing rather than a new page ticket.
async function fetchAuthenticatedManifest(
  url: string, path: string, credentials: RequestCredentials, backend: BackendKey = HOME_BACKEND,
): Promise<Response> {
  const paired = hasPairedSession(backend);
  // same-origin credentials is the default for a same-origin request,
  // but state it: this fetch is the cookie's whole delivery path, and a
  // future caller passing a different mode would silently unauthenticate
  // the page. A PAIRED page also presents its stored session credential:
  // its one-time ticket is long spent and its cookie dies with the
  // backend launch that planted it, so after a restart the session
  // credential is the only thing that still names this page.
  let resp = await fetchPairedComputer(backend, networkFetch, url, {
    credentials,
    // The PATH, never the absolute URL: a device proof binds
    // (method, path) and the backend compares `r.URL.Path`
    // (internal/identity/deviceproof.go), so a cross-origin fetch signs
    // exactly what a same-origin one signs.
    headers: await pairedSessionHeaders('GET', path, backend),
  });
  if (!resp.ok && CREDENTIAL_REFUSED_STATUSES.has(resp.status) && hasPairedSession(backend)) {
    // The stored access credential may simply have aged out between
    // visits; the refresh exchange decides whether the session is dead.
    // One renewal, one retry. A definitive refusal clears the store and
    // asks for pairing; an inconclusive exchange stays retryable.
    if (await renewPairedSession(networkFetch, backend)) {
      resp = await fetchPairedComputer(backend, networkFetch, url, {
        credentials,
        // A fresh proof: proofs are single-use, so the one the first
        // attempt carried is spent.
        headers: await pairedSessionHeaders('GET', path, backend),
      });
    } else if (hasPairedSession(backend)) {
      // A network failure, throttling, or pending confirmation is not
      // evidence that the renewal credential is dead. Keep retrying.
      throw new Error('paired session renewal unavailable');
    }
  }
  if (!resp.ok) {
    if (!CREDENTIAL_REFUSED_STATUSES.has(resp.status)) {
      // Transient: the server is up but not serving the manifest yet
      // (503 readiness gate, 500 startup failure) or something in
      // between failed. The cookie the exchange already set is still
      // the right one — the server issues it before those gates run.
      throw new Error(`bootstrap fetch failed: HTTP ${resp.status}`);
    }
    throw new BootstrapRejectedError(resp.status, paired);
  }
  return resp;
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
  // ./homeEndpoint.ts is the one seam between "this page's origin" and
  // "the home backend's". It is the identity for every client that was
  // served its bundle by the backend it talks to, so this line is the
  // same request it has always been for all of them.
  const url = homeUrl(ticket === ''
    ? '/bootstrap.json'
    : `/bootstrap.json?${PAGE_TICKET_PARAM}=${encodeURIComponent(ticket)}`);
  const resp = await fetchAuthenticatedManifest(url, '/bootstrap.json', homeCredentials());
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
  validateWsUrl(data.wsUrl, originPartsOf(computerResponseURL(resp, url)) ?? homeOriginParts());
  await observePairedComputerBootstrap(HOME_BACKEND, data.backendId, data.routes);
  data.remote = data.remote === true;
  // Locality is this page's half of the capability answer: a page served
  // over loopback IS the owner's screen, and a page served over the
  // network holds only what it paired for. Published before the harness
  // latch below, which reads it at arm time.
  setPageGrantsFromBootstrap(data.remote);
  setHarnessPageMarkerFromBootstrap(data.pageMarker);
  setPasskeysAvailableFromBootstrap(data.passkeysAvailable === true);
  // The harness bridge registers its page synchronously when this latch
  // flips. Publish the marker first so that first-load registration cannot
  // race with the waiter callback.
  setHarnessSessionFromBootstrap(data.harness === true);
  // Re-validated on every manifest resolution, which is what makes a
  // mid-session generation re-mint (a restored backend) observable on
  // the reconnect refetch rather than at the next app launch.
  setBackendIdentityFromBootstrap(data.backendId, data.replicaGeneration, data.backendName);
  // The attached-backend list is re-read on every manifest resolution —
  // including the reconnect refetch — so a backend added or removed from
  // Settings takes effect without a reload. The publisher notifies only on
  // a real change, so a reconnect that repeats the list sweeps nothing. An
  // ABSENT list is a single-backend page and not an error: the attached
  // routes answer loopback only, so an off-host page is never given one.
  publishManifestBackends(readBackendDescriptors(data.backends));
  // Remove the spent ticket from history, Referer, and Performance
  // Resource Timing entries. The cookie carries the session from here,
  // so a reload of the scrubbed URL still boots. Skipped for a
  // webview-hosted page, whose ticket was never on the URL to begin
  // with, and when history.replaceState isn't available (older
  // happy-dom builds, weird host pages).
  if (
    ticket !== '' &&
    !isWebviewHosted() &&
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
// The `--connect` stub used to be an exemption: it injected a manifest
// naming a remote backend and the page opened a cross-origin socket. It
// now serves its own origin's manifest and carries the socket to the
// upstream itself, so no client class opts out of the check.
//
// What the check compares against is a PARAMETER, and that is the phone
// wave's whole change here. Origin still means "the one authority this
// manifest is allowed to describe" — it is just that a shell page's
// backend is not the page's own origin, and an ATTACHED backend's is
// neither. `expected` defaults to the home backend's
// (./homeEndpoint.ts, which answers this document's origin whenever no
// endpoint is set), so every existing call is unchanged; the attached
// fetcher below passes that backend's own.
export function validateWsUrl(
  wsUrl: string,
  expected: { protocol: string; host: string } | null = homeOriginParts(),
): void {
  let parsed: URL;
  try {
    parsed = new URL(wsUrl);
  } catch {
    throw new Error(`bootstrap wsUrl invalid: ${clampString(wsUrl)}`);
  }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    throw new Error(`bootstrap wsUrl scheme not ws/wss: ${clampString(parsed.protocol)}`);
  }
  if (parsed.username || parsed.password || parsed.hash) {
    throw new Error('bootstrap wsUrl cannot contain credentials or a fragment');
  }
  if (expected === null) {
    // An origin requirement we cannot evaluate is a requirement we
    // cannot meet. Unreachable in practice — the only caller fetched a
    // relative '/bootstrap.json', which already needs a document base —
    // but it must fail closed, not open.
    throw new Error('bootstrap wsUrl cannot be origin-checked: no document origin');
  }
  if (!wsUrlMatchesPageOrigin(wsUrl, expected)) {
    throw new Error(`bootstrap wsUrl not same-origin: ${clampString(wsUrl)}`);
  }
}

// An attached backend's manifest fetch: the same exchange this module
// performs for the page's own backend, against that backend's own path
// (`/bootstrap/<id>.json`, the `clientmode` proxy of spec §10).
//
// No ticket rides it. The local process in front of an attached backend
// holds that backend's credential and presents it upstream itself, which
// is the whole reason the desktop realization is a proxy: CSP stays
// `'self'` and no credential enters page script. On a phone the
// per-backend session slot supplies one instead (./deviceSession.ts,
// keyed by the same registry id).
//
// `validateWsUrl` still applies; what it compares against is the
// descriptor's OWN origin. Under the desktop proxy that is the page's,
// because every attached backend is same-origin by construction there,
// and a manifest naming another authority was tampered with in flight
// exactly as it would be for home. On a phone each descriptor is
// absolute (./manifestBackends.ts, `descriptorForAttachedId`) and the
// check follows it, so the rule is the same sentence with a different
// subject rather than an exemption.
setBackendManifestFetcher(async (descriptor) => {
  const remote = originPartsOf(descriptor.bootstrapUrl);
  const path = new URL(descriptor.bootstrapUrl, window.location.href).pathname;
  const resp = await fetchAuthenticatedManifest(
    descriptor.bootstrapUrl, path, remote === null ? 'same-origin' : 'omit', descriptor.id,
  );
  const data = (await resp.json()) as Partial<Bootstrap>;
  const wsUrl = typeof data.wsUrl === 'string' ? data.wsUrl : descriptor.wsUrl;
  if (descriptor.backendId && data.backendId !== descriptor.backendId) {
    throw new Error('This address belongs to a different computer. Pair with that computer before connecting.');
  }
  validateWsUrl(wsUrl, originPartsOf(computerResponseURL(resp, descriptor.bootstrapUrl)) ?? remote ?? homeOriginParts());
  await observePairedComputerBootstrap(descriptor.id, data.backendId, data.routes);
  setBackendIdentityFromBootstrap(
    data.backendId,
    data.replicaGeneration,
    data.backendName ?? descriptor.name,
    descriptor.id,
  );
  setCarriedSessionScopes(descriptor.id, data.sessionScopes);
  return { ...data, wsUrl };
});
