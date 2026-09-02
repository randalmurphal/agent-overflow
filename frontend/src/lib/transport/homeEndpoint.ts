// Where the home backend actually is, for a client whose PAGE is served
// from somewhere else (docs/specs/remote-access.md §10, "One seam, two
// realizations").
//
// Every client before wave 6f-c was served its bundle by the backend it
// then talked to, so every home-backend URL in this directory could be
// RELATIVE and every one of them was: `/bootstrap.json`, `/auth/*`, the
// minted attachment URLs, the manifest's `wsUrl`. The phone shell breaks
// that assumption and only that one — it serves the same bundle from its
// own fixed origin (`https://shell.agent-overflow.invalid`, which can
// never resolve on a network) and the backend lives on the tailnet. So
// this module is the ONE seam between "the page's origin" and "the home
// backend's origin", and the whole rest of the client keeps writing the
// paths it always wrote.
//
// **Empty means same-origin, and that is the desktop's answer forever.**
// `homeUrl` returns its argument unchanged and `homeWsUrl` is the
// identity, so the embedded webview, `--connect` and a paired browser
// issue byte-identical requests to the ones they issued before this file
// existed. There is no client-class branch anywhere below it.
//
// **`window.__aoHomeEndpoint` is the shell's door, read once at module
// load.** The shell sets it in the page before the bundle evaluates; the
// cross-origin e2e spec sets the same global through
// `page.addInitScript`. One door, so the thing the test exercises is the
// thing the shell uses — and reading it at module load is what lets the
// first fetch of the boot already be addressed correctly, since there is
// no await between module evaluation and `defaultBootstrap`.
//
// **Credentials are OMITTED once an endpoint is set.** A phone presents
// `X-AO-Session` plus its device-key proof on every request; it holds no
// cookie for the backend's origin and could not be sent one. Saying
// `omit` rather than leaving the default is the same discipline the rest
// of this directory keeps about credential modes: the default changing
// under us would be a silent break in exactly one boot.

import { HOME_BACKEND, type BackendKey } from './backendKey';

/** localStorage key holding every backend's endpoint, home under `''`. */
const ENDPOINTS_STORE_KEY = 'agent-overflow:backendEndpoints';

let endpoint = '';

/**
 * Normalise a shell-supplied endpoint to a bare origin, or throw.
 *
 * Only http/https are accepted, and only the ORIGIN is kept: a path, a
 * query or a fragment on this value would be silently prepended to every
 * route in the app, which is a class of bug that is invisible until a
 * route 404s for a reason nobody can read off the request.
 */
function normaliseEndpoint(origin: string): string {
  const parsed = new URL(origin);
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new Error(`home endpoint scheme not http/https: ${parsed.protocol}`);
  }
  if (parsed.host === '') throw new Error('home endpoint names no host');
  return parsed.origin;
}

/**
 * Point every home-backend URL at `origin` instead of at this page.
 *
 * Called at boot by the shell, before anything fetches, and by the
 * pairing flow when a scanned payload names the endpoint it is pairing
 * with — which is also before that flow's first request. Nothing else
 * calls it: an endpoint that moved mid-session would leave in-flight
 * requests addressed to the old one and a socket dialed at neither.
 *
 * Throws on a value that is not an absolute http(s) origin. Loud is
 * right here: falling back to same-origin would leave a phone quietly
 * fetching `/bootstrap.json` from `shell.agent-overflow.invalid`, whose
 * whole point is that it resolves nowhere.
 */
export function setHomeEndpoint(origin: string): void {
  endpoint = normaliseEndpoint(origin);
}

/** The home backend's origin, or `''` when it is this page's own. */
export function homeEndpoint(): string {
  return endpoint;
}

/**
 * Whether this client's page and its home backend are different origins.
 *
 * The one thing a surface may branch on, and it means exactly what it
 * says — not "is this a phone". The pairing screen reads it because a
 * page that is not its backend's cannot compare a payload's endpoint
 * against its own origin and must ADOPT it instead.
 */
export function hasHomeEndpoint(): boolean {
  return endpoint !== '';
}

/**
 * Address one home-backend route.
 *
 * Unchanged when no endpoint is set, which is what makes every existing
 * call site correct without being touched. An argument that is already
 * absolute is returned as it came: the caller resolved it somewhere else
 * on purpose, and prefixing an origin onto an origin is never right.
 */
export function homeUrl(path: string): string {
  if (endpoint === '' || path === '') return path;
  if (path.includes('://')) return path;
  return path.startsWith('/') ? endpoint + path : `${endpoint}/${path}`;
}

/**
 * The credentials mode a home-backend fetch uses.
 *
 * `same-origin` is today's answer and stays it: the `--connect` stub
 * checks its own page cookie before relaying, so a request that dropped
 * credentials would be refused there while working everywhere else. With
 * an endpoint set there is no cookie to send — the session rides
 * `X-AO-Session` and the device-key proof — and asking for one would only
 * teach the backend's CORS answer to carry a credentials flag it should
 * never carry.
 */
export function homeCredentials(): RequestCredentials {
  return endpoint === '' ? 'same-origin' : 'omit';
}

/**
 * The origin every home-backend URL resolves against: the endpoint when
 * one is set, else this document's own. Null when neither exists, so a
 * caller that must fail closed can.
 */
export function homeOriginParts(): { protocol: string; host: string } | null {
  if (endpoint !== '') return originPartsOf(endpoint);
  if (typeof window === 'undefined' || typeof window.location === 'undefined') return null;
  return { protocol: window.location.protocol, host: window.location.host };
}

/**
 * Split any absolute URL into the two halves an origin comparison needs.
 * Null when it does not parse; `ws:`/`wss:` are NOT mapped here, since
 * the one comparison that needs the mapping owns it (`bootstrap.ts`).
 */
export function originPartsOf(url: string): { protocol: string; host: string } | null {
  try {
    const parsed = new URL(url);
    return { protocol: parsed.protocol, host: parsed.host };
  } catch {
    return null;
  }
}

/**
 * Carry a manifest's `wsUrl` onto the endpoint.
 *
 * It rewrites exactly two shapes and leaves everything else alone: a
 * RELATIVE url, and one naming this PAGE's own host. Both are a manifest
 * describing a socket at the origin that served the document, which is
 * the one thing a shell page can be sure is wrong. An absolute url naming
 * some other host is an ATTACHED backend's own endpoint (`backends.ts`
 * holds one client per machine, and on a phone each of them is remote),
 * so rewriting it would point every attached machine's socket at home.
 *
 * The scheme comes from the endpoint rather than from the manifest: a
 * TLS endpoint takes `wss:` and a plain one `ws:`, so a manifest cannot
 * move a secure client onto a cleartext socket.
 */
export function homeWsUrl(wsUrl: string): string {
  if (endpoint === '') return wsUrl;
  const base = new URL(endpoint);
  const scheme = base.protocol === 'http:' ? 'ws:' : 'wss:';
  let parsed: URL;
  try {
    parsed = new URL(wsUrl, base);
  } catch {
    return wsUrl;
  }
  const pageHost = typeof window !== 'undefined' && typeof window.location !== 'undefined'
    ? window.location.host.toLowerCase()
    : '';
  const absolute = /^[a-z][a-z0-9+.-]*:/i.test(wsUrl);
  if (absolute && parsed.host.toLowerCase() !== pageHost) return wsUrl;
  return `${scheme}//${base.host}${parsed.pathname}${parsed.search}`;
}

// ---------------------------------------------------------------------------
// The stored endpoint of every backend this client holds a session for
// ---------------------------------------------------------------------------

// A phone with N backends holds N session slots (`deviceSession.ts`,
// `sessionStoreKey`) and needs one more fact per slot that a browser never
// needed: WHERE that backend is. A browser's answer was its own origin.
//
// One JSON map, keyed by the same registry id the session slots are keyed
// by, with home under `''` — the same convention, so "the page's own
// backend" is spelled once in this app rather than twice. Read at boot to
// set the home endpoint and to rebuild the attached descriptors; nothing
// polls it.

function readEndpointMap(): Record<string, string> {
  if (typeof localStorage === 'undefined') return {};
  let raw: string | null;
  try {
    raw = localStorage.getItem(ENDPOINTS_STORE_KEY);
  } catch {
    return {};
  }
  if (!raw) return {};
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {};
    const out: Record<string, string> = {};
    for (const [id, value] of Object.entries(parsed as Record<string, unknown>)) {
      // Validated rather than trusted, the same rule
      // `manifestBackends.readBackendDescriptors` states: an entry this
      // build cannot read is dropped, never coerced into a connection to
      // somewhere unintended. A registry id may hold no space — it is the
      // prefix of every path-keyed composite key.
      if (typeof value !== 'string' || id.includes(' ')) continue;
      try {
        out[id] = normaliseEndpoint(value);
      } catch {
        // A damaged entry drops out; the rest of the map still works.
      }
    }
    return out;
  } catch {
    return {};
  }
}

function writeEndpointMap(map: Record<string, string>): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(ENDPOINTS_STORE_KEY, JSON.stringify(map));
  } catch {
    // Storage refused (private mode, quota). The endpoints then live for
    // this launch only, which degrades to "pair again next launch".
  }
}

/** Every backend endpoint this client has stored, home under `''`. */
export function storedBackendEndpoints(): Record<string, string> {
  return readEndpointMap();
}

/** The stored endpoint for one backend, or `''` when there is none. */
export function storedBackendEndpoint(backend: BackendKey = HOME_BACKEND): string {
  return readEndpointMap()[backend] ?? '';
}

/**
 * Remember where one backend is, beside the session slot that names it.
 * Written by the pairing paths at the moment the endpoint is known and
 * before the credential is stored, so a stored session can never outlive
 * the knowledge of where to present it.
 */
export function storeBackendEndpoint(backend: BackendKey, origin: string): void {
  const map = readEndpointMap();
  map[backend] = normaliseEndpoint(origin);
  writeEndpointMap(map);
}

/**
 * The host part of an endpoint, or `''` when it names none.
 *
 * The placeholder name an attached machine carries until its manifest
 * publishes its own: an address is the one thing every backend has from
 * the moment it is stored, and it is what a person just typed or scanned.
 * It lives beside the map it is read out of so both readers — the
 * descriptor rebuild and the attach flow — spell it once.
 */
export function endpointHost(endpoint: string): string {
  try {
    return new URL(endpoint).host;
  } catch {
    return '';
  }
}

/** Forget one backend's endpoint. Detaching a machine's last step. */
export function forgetBackendEndpoint(backend: BackendKey): void {
  const map = readEndpointMap();
  if (!(backend in map)) return;
  delete map[backend];
  writeEndpointMap(map);
}

// The shell's and the e2e spec's shared door, read ONCE at module load.
// A shell page evaluates its own script before the bundle; the spec sets
// the same global through `page.addInitScript`. Invalid is warned about
// and ignored rather than thrown: a bundle that refused to evaluate would
// take the error surface down with it, and same-origin is the answer that
// at least renders something.
try {
  const declared = (globalThis as { __aoHomeEndpoint?: unknown }).__aoHomeEndpoint;
  if (typeof declared === 'string' && declared !== '') setHomeEndpoint(declared);
} catch (err) {
  console.warn('transport: ignoring an unusable __aoHomeEndpoint', err);
}

/** Test seam: forget the endpoint this module read or was told. */
export function __resetHomeEndpointForTest(): void {
  endpoint = '';
}
