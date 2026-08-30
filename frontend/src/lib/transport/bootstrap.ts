// Bootstrap manifest handling for the transport: the /bootstrap.json
// fetch, the per-tab token stash that survives URL scrubbing, and the
// WS-URL validation that keeps a hijacked manifest from pivoting the
// connection to an arbitrary scheme.

import { setViewOnlySessionFromBootstrap } from './runMode';
import { setHarnessPageMarkerFromBootstrap, setHarnessSessionFromBootstrap } from './harnessMode';
import { setBackendIdentityFromBootstrap } from './backendIdentity';
import { clampString } from './frames';

// RunMode marks how the SPA is attached to its backend:
//   - 'local'    — desktop binary booted a local transport in the same
//                  process. The default whenever the bootstrap omits
//                  the field, since /bootstrap.json on the local
//                  transport doesn't carry mode.
//   - 'client'   — desktop binary launched with --connect; the local
//                  process owns only a stub HTTP server and the SPA
//                  RPCs flow to a remote backend. Local-only settings
//                  panels must hide / placeholder in this mode.
//   - 'headless' — reserved for the WSL launcher path. Not currently
//                  emitted by any boot flow (the Windows-side WebView2
//                  bootstrap-injected page doesn't inject mode), but
//                  defined here so a future Phase D bootstrap can mark
//                  itself without an enum widening.
export type RunMode = 'local' | 'client' | 'headless';

// BootstrapRejectedError marks the one bootstrap failure that retrying
// cannot fix: the server answered, and refused our credential. Tokens
// are minted per backend launch (internal/transport/server.go
// handleBootstrap answers a bad `?t=` with 404, deliberately
// indistinguishable from "no such path"), so a remote/LAN client whose
// backend restarted holds a token that will never be honoured again —
// only reopening the share link mints a new one. Distinct from a
// transient failure (network error, the 503 readiness gate, the 500
// startup-failure page) so the transport can surface an actionable
// state instead of a silent forever-loop.
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

// isLoopbackHostname reports whether a document host names this machine.
// Exported pure so the predicate is testable without a document.
//
// The distinction matters exactly once: a refused credential is only
// terminal for a session that cannot mint a new one. The embedded
// webview and the `--connect` stub always load from loopback and are
// handed a live token by the shell that owns the backend, so a refusal
// there is a transient boot race worth retrying. A page served over the
// network got its token from a share link, and only re-opening that link
// (a fresh page load) can produce another one.
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

// Bootstrap is the JSON the SPA fetches at /bootstrap.json on first load.
// Mirror the Go-side shape (internal/transport/server.go Bootstrap).
// `mode` is optional on the wire — only the clientmode injection
// emits it today; the local /bootstrap.json path leaves it absent and
// the SPA treats absence as 'local'.
export interface Bootstrap {
  wsUrl: string;
  token: string;
  mode?: RunMode;
  remote?: boolean;
  /**
   * Durable UI-state client identity minted by the local shell
   * (--connect injection only; the embedded-webview paths carry it as
   * the ?cid= URL param instead). Consumed by stores/appStorage.ts at
   * module init, not by the transport itself.
   */
  clientId?: string;
  /**
   * Stable per-store UUID keying the client-side thread replica, plus
   * the generation that is re-minted whenever the backend's history
   * counters lose continuity (docs/architecture/thread-replica-sync.md §3.3).
   * Absent from the `--connect` stub's injected manifest — absence
   * disables the replica rather than sharing another backend's database.
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

// Session-scoped stash for the bootstrap token. The token arrives once
// as `?t=` and is immediately scrubbed from the URL (see replaceState
// below), so without this stash any reload — browser F5, the Ctrl+R
// uikeys binding in the embedded webview, a Playwright page.reload() —
// loses the token and every subsequent /bootstrap.json fetch 404s.
// sessionStorage is per-tab and dies with it, matching the token's
// soft-secret posture. Access is fault-tolerant: sandboxed frames and
// some embeddings throw on storage access, and a broken stash must
// degrade to "reload needs the tokened URL again", not a crash.
const TOKEN_STORAGE_KEY = 'ao:bootstrap-token';

function readStoredToken(): string {
  try {
    return window.sessionStorage.getItem(TOKEN_STORAGE_KEY) ?? '';
  } catch {
    return '';
  }
}

function writeStoredToken(token: string): void {
  try {
    window.sessionStorage.setItem(TOKEN_STORAGE_KEY, token);
  } catch {
    // Stash unavailable — reloads will need the tokened URL again.
  }
}

// Default bootstrap fetcher: read from window.__AO_BOOTSTRAP__ (set by
// Phase F's `--connect` flow) or fall back to `/bootstrap.json?t=<token>`
// where the token comes from `?t=` in window.location.search, or — on a
// reload, after the URL was scrubbed — from sessionStorage. This runs
// the first time anyone calls `ensureConnected`; subsequent calls reuse
// the cached promise.
//
// The injected path CAN observe a refusal, via its own origin: the
// `--connect` stub serves /bootstrap.json by probing the upstream
// backend with its configured token from Go, where CORS does not apply
// (clientmode.handleBootstrap). The page itself could never ask the
// upstream — a cross-origin fetch dies on CORS and a rejected WS
// upgrade is a bare 1006 — so `revalidate` is what routes a reconnect
// outage's refetch through the stub instead of short-circuiting on the
// injected global. First load keeps the zero-round-trip injected
// answer.
export async function defaultBootstrap(opts?: { revalidate?: boolean }): Promise<Bootstrap> {
  const injected = (globalThis as { __AO_BOOTSTRAP__?: Bootstrap }).__AO_BOOTSTRAP__;
  if (injected && typeof injected.wsUrl === 'string' && typeof injected.token === 'string') {
    if (opts?.revalidate) {
      // Ask the stub for the upstream's verdict. 404 → the same
      // BootstrapRejectedError a browser session gets, which is what
      // lets the credentialDead latch cover --connect clients too; the
      // manifest a 200 returns is the stub's own (wsUrl as configured,
      // mode:"client"), so nothing about the session shifts.
      //
      // The fetch itself is same-origin (the stub serves both the page
      // and /bootstrap.json); it is the UPSTREAM BACKEND the wsUrl names
      // that is cross-origin, which is why requireSameOrigin=false. The
      // trust anchor is the local stub, so pin the answer to what it
      // injected at page load: a revalidate must confirm the session,
      // never retarget it (2026-08-25 security review, finding 7).
      const revalidated = await fetchManifest(injected.token, '', false);
      if (revalidated.wsUrl !== injected.wsUrl) {
        throw new Error(
          `bootstrap revalidate returned a different wsUrl than the injected manifest (${revalidated.wsUrl} vs ${injected.wsUrl})`,
        );
      }
      return revalidated;
    }
    validateWsUrl(injected.wsUrl, false);
    const normalized = { ...injected, mode: normalizeRunMode(injected.mode), remote: injected.remote === true };
    setViewOnlySessionFromBootstrap(normalized.remote);
    setHarnessPageMarkerFromBootstrap(normalized.pageMarker);
    setHarnessSessionFromBootstrap(normalized.harness === true);
    setBackendIdentityFromBootstrap(normalized.backendId, normalized.replicaGeneration);
    return normalized;
  }
  const search = typeof window !== 'undefined' ? window.location.search : '';
  const params = new URLSearchParams(search);
  const urlToken = params.get('t') ?? '';
  const token = urlToken !== '' ? urlToken : readStoredToken();
  // Nothing injected this manifest, so the page was served by the
  // transport itself (embedded webview, WSL launcher window, LAN
  // browser, or dev — where the Go server proxies Vite and the page
  // origin is still the Go server's). Its wsUrl is derived from this
  // request's own Host header, so it must name this very origin.
  return fetchManifest(token, urlToken, true);
}

// fetchManifest is the one /bootstrap.json fetch + validation path,
// shared by the browser flow (token from URL or sessionStorage) and the
// injected flow's revalidation (token from the injected manifest, and
// the stub answers). urlToken gates the history scrub — only a page
// that actually carries ?t= has anything to remove.
//
// requireSameOrigin is the CALLER's statement about which flow this is,
// not something read out of the response. See validateWsUrl.
async function fetchManifest(
  token: string,
  urlToken: string,
  requireSameOrigin: boolean,
): Promise<Bootstrap> {
  const url = `/bootstrap.json?t=${encodeURIComponent(token)}`;
  const resp = await fetch(url, { credentials: 'same-origin' });
  if (!resp.ok) {
    if (!CREDENTIAL_REFUSED_STATUSES.has(resp.status)) {
      // Transient: the server is up but not serving the manifest yet
      // (503 readiness gate, 500 startup failure) or something in
      // between failed. The stashed token is still the right one.
      throw new Error(`bootstrap fetch failed: HTTP ${resp.status}`);
    }
    // The stashed token is deliberately KEPT on a refusal. Dropping it
    // buys nothing — re-presenting a stale token yields the identical
    // 404 — and it destroys the one copy that would let a page reload
    // recover from a refusal that wasn't real (a proxy blip answering
    // 404 for a token the server still honours).
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
  if (typeof data.wsUrl !== 'string' || typeof data.token !== 'string') {
    throw new Error('bootstrap response missing wsUrl/token');
  }
  validateWsUrl(data.wsUrl, requireSameOrigin);
  data.mode = normalizeRunMode(data.mode);
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
  // Stash the server-confirmed token so the tab survives reloads once
  // the URL is scrubbed below.
  writeStoredToken(data.token);
  // Removes the token from history, Referer, and Performance Resource
  // Timing entries. Same-origin redirects and tab-history scrubbing both
  // benefit. Skip when history.replaceState isn't available (older
  // happy-dom builds, weird host pages).
  if (
    typeof window !== 'undefined' &&
    typeof window.history !== 'undefined' &&
    typeof window.history.replaceState === 'function' &&
    urlToken !== ''
  ) {
    try {
      const retained = new URLSearchParams(window.location.search);
      retained.delete('t');
      const suffix = retained.toString();
      window.history.replaceState(null, '', window.location.pathname + (suffix ? `?${suffix}` : '') + window.location.hash);
    } catch {
      // Some embeddings throw on replaceState; the token-on-URL is
      // already a soft secret, so swallowing is acceptable.
    }
  }
  return data;
}

// normalizeRunMode coerces an incoming mode value to the typed enum.
// Anything outside the known set falls back to 'local' — same as
// absent. Keeping this loose-and-default-safe is intentional: a future
// backend that sends an unrecognised mode shouldn't crash the SPA;
// the worst case is a remote-mode panel rendering when the user is
// actually local, which is benign.
function normalizeRunMode(mode: unknown): RunMode {
  if (mode === 'client' || mode === 'headless' || mode === 'local') return mode;
  return 'local';
}

// wsUrlMatchesPageOrigin reports whether wsUrl addresses the same origin
// the page was served from. Pure and exported so the comparison is
// testable against origins this document will never have — the same
// split as isLoopbackHostname / pageServedOverLoopback above.
//
// Origin here is scheme + host + PORT, with ws:/wss: mapped onto their
// http:/https: counterparts. Host alone would not do: a second listener
// on the same machine is a different security principal, and on a LAN
// bind it need not be ours at all. The scheme pairing is what stops a
// TLS-fronted page being downgraded onto a cleartext socket. Explicit
// default ports normalise away on both sides (ws:/http: share 80,
// wss:/https: share 443), so the two spellings still match.
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
// WebSocket somewhere it must not go. Two independent checks:
//
//  1. Scheme — always. A manifest can't pivot the connection to an
//     arbitrary URL handler, whichever flow produced it.
//  2. Origin — only when the CALL SITE asks for it. A manifest fetched
//     from /bootstrap.json on the transport's own origin carries a wsUrl
//     the server derived from that very request's Host header
//     (internal/transport/server.go deriveWSURL), so anything naming
//     another authority was tampered with in flight — and honouring it
//     would hand the bootstrap token to the attacker's socket.
//
// The origin requirement is a PARAMETER rather than something inferred
// from the manifest, and that is the whole security property: the two
// legitimately cross-origin flows are both `--connect`
// (internal/clientmode), where the local stub injects
// window.__AO_BOOTSTRAP__ naming a remote backend and serves the same
// manifest again from its own /bootstrap.json for the reconnect
// revalidation. What distinguishes them is HOW the manifest reached the
// page — an out-of-band injection by the shell that owns this process,
// which already implies script execution — never a field inside the
// manifest. Reading the exemption off `mode: "client"` would let any
// spoofed manifest exempt itself by saying so.
export function validateWsUrl(wsUrl: string, requireSameOrigin: boolean): void {
  let parsed: URL;
  try {
    parsed = new URL(wsUrl);
  } catch {
    throw new Error(`bootstrap wsUrl invalid: ${clampString(wsUrl)}`);
  }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    throw new Error(`bootstrap wsUrl scheme not ws/wss: ${clampString(parsed.protocol)}`);
  }
  if (!requireSameOrigin) return;
  if (typeof window === 'undefined' || typeof window.location === 'undefined') {
    // A same-origin requirement we cannot evaluate is a requirement we
    // cannot meet. Unreachable in practice — this branch's only caller
    // fetched a relative '/bootstrap.json', which already needs a
    // document base — but it must fail closed, not open.
    throw new Error('bootstrap wsUrl cannot be origin-checked: no document origin');
  }
  if (!wsUrlMatchesPageOrigin(wsUrl, window.location)) {
    throw new Error(`bootstrap wsUrl not same-origin: ${clampString(wsUrl)}`);
  }
}

// appendToken adds `?token=<value>` to the WS URL. Handles URLs that
// already carry query params via the URL constructor.
export function appendToken(wsUrl: string, token: string): string {
  try {
    const parsed = new URL(wsUrl);
    parsed.searchParams.set('token', token);
    return parsed.toString();
  } catch {
    // Relative or otherwise un-parseable — fall back to a plain
    // concatenation. We bias toward letting the browser's WS
    // implementation reject a bad URL rather than silently mutating it.
    const sep = wsUrl.includes('?') ? '&' : '?';
    return `${wsUrl}${sep}token=${encodeURIComponent(token)}`;
  }
}
