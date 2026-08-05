// Bootstrap manifest handling for the transport: the /bootstrap.json
// fetch, the per-tab token stash that survives URL scrubbing, and the
// WS-URL validation that keeps a hijacked manifest from pivoting the
// connection to an arbitrary scheme.

import { setViewOnlySessionFromBootstrap } from './runMode';
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

function clearStoredToken(): void {
  try {
    window.sessionStorage.removeItem(TOKEN_STORAGE_KEY);
  } catch {
    // Nothing to clear if the stash itself is unreadable.
  }
}

// Default bootstrap fetcher: read from window.__AO_BOOTSTRAP__ (set by
// Phase F's `--connect` flow) or fall back to `/bootstrap.json?t=<token>`
// where the token comes from `?t=` in window.location.search, or — on a
// reload, after the URL was scrubbed — from sessionStorage. This runs
// the first time anyone calls `ensureConnected`; subsequent calls reuse
// the cached promise.
//
// Known gap on the injected path: `--connect` clients never issue this
// fetch, so a refused credential has no observable signal there. The
// browser WebSocket API reports a rejected upgrade as a bare 1006, and
// probing the remote `/bootstrap.json` cross-origin fails on CORS +
// the loopback Host guard (server.go) — i.e. it is indistinguishable
// from "server down" too. Those clients stay in the honest-but-vague
// 'reconnecting' state; closing the gap needs a CORS-safe backend
// affordance, which is a wire-surface change, not a client fix.
export async function defaultBootstrap(): Promise<Bootstrap> {
  const injected = (globalThis as { __AO_BOOTSTRAP__?: Bootstrap }).__AO_BOOTSTRAP__;
  if (injected && typeof injected.wsUrl === 'string' && typeof injected.token === 'string') {
    validateWsUrl(injected.wsUrl);
    const normalized = { ...injected, mode: normalizeRunMode(injected.mode), remote: injected.remote === true };
    setViewOnlySessionFromBootstrap(normalized.remote);
    return normalized;
  }
  const search = typeof window !== 'undefined' ? window.location.search : '';
  const params = new URLSearchParams(search);
  const urlToken = params.get('t') ?? '';
  const token = urlToken !== '' ? urlToken : readStoredToken();
  const url = `/bootstrap.json?t=${encodeURIComponent(token)}`;
  const resp = await fetch(url, { credentials: 'same-origin' });
  if (!resp.ok) {
    if (!CREDENTIAL_REFUSED_STATUSES.has(resp.status)) {
      // Transient: the server is up but not serving the manifest yet
      // (503 readiness gate, 500 startup failure) or something in
      // between failed. The stashed token is still the right one.
      throw new Error(`bootstrap fetch failed: HTTP ${resp.status}`);
    }
    if (urlToken === '' && token !== '') {
      // The stashed token was refused — stale after a backend restart
      // (tokens are minted per boot). Drop it so retries surface the
      // real "no valid token" state instead of re-presenting it.
      clearStoredToken();
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
  if (typeof data.wsUrl !== 'string' || typeof data.token !== 'string') {
    throw new Error('bootstrap response missing wsUrl/token');
  }
  validateWsUrl(data.wsUrl);
  data.mode = normalizeRunMode(data.mode);
  data.remote = data.remote === true;
  setViewOnlySessionFromBootstrap(data.remote);
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
      window.history.replaceState(null, '', window.location.pathname + window.location.hash);
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

// validateWsUrl rejects bootstrap responses pointing the client at a
// scheme other than ws:/wss:. A boostrap fetch is over same-origin
// HTTP(S), but defending here means a hijacked bootstrap response can't
// pivot the WS connection to an arbitrary URL handler.
function validateWsUrl(wsUrl: string): void {
  let parsed: URL;
  try {
    parsed = new URL(wsUrl);
  } catch {
    throw new Error(`bootstrap wsUrl invalid: ${clampString(wsUrl)}`);
  }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    throw new Error(`bootstrap wsUrl scheme not ws/wss: ${clampString(parsed.protocol)}`);
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
