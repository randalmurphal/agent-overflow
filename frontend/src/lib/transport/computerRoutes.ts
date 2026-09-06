// Native connection routes are frontend metadata, bound to one pairing. They
// never share a storage write or lock with credential renewal. Every delayed
// result rechecks that pairing before publishing or saving an address.
import type { BackendKey } from './backendKey';
import { mergeComputerRoutes, repairComputerRouteCandidates, type ComputerRoute } from './computerRoute';
import { canVerifyComputerRoutes, verifyComputerRoute } from '../native/computerRouteProbe';
import { pinnedFetch } from '../native/networkHttp';

export interface ComputerRouteContext {
  backend: BackendKey;
  sessionId: string;
  backendId: string;
  primary: ComputerRoute;
  current(): boolean;
  beforeRequest?(): void;
}
interface Profile {
  sessionId: string;
  backendId: string;
  primary: ComputerRoute;
  routes: ComputerRoute[];
  lastEndpoint?: string;
}
interface State {
  context: ComputerRouteContext;
  profile: Profile;
  active: ComputerRoute;
  failed: boolean;
  revision: number;
  selection?: Promise<ComputerRoute>;
  cancel?: AbortController;
  retryAt: number;
}

const KEY = 'agent-overflow:computerRoutes:';
const states = new Map<BackendKey, State>();
const responseURLs = new WeakMap<Response, string>();
const sameRoute = (a: ComputerRoute, b: ComputerRoute) => a.endpoint === b.endpoint && (a.certFingerprint || '') === (b.certFingerprint || '');
const bound = (profile: Profile, context: ComputerRouteContext) => profile.sessionId === context.sessionId && profile.backendId === context.backendId && sameRoute(profile.primary, context.primary);

function readProfile(context: ComputerRouteContext): Profile {
  try {
    const raw = localStorage.getItem(KEY + encodeURIComponent(context.backend));
    if (raw && raw.length <= 64 * 1024) {
      const profile = JSON.parse(raw) as Profile;
      if (profile?.primary && bound(profile, context)) return { ...profile, routes: mergeComputerRoutes([], profile.routes) };
    }
  } catch { /* Corrupt route metadata cannot invalidate the actual pairing. */ }
  return { sessionId: context.sessionId, backendId: context.backendId, primary: context.primary, routes: [] };
}

function stateFor(context: ComputerRouteContext): State {
  let state = states.get(context.backend);
  if (!state || !bound(state.profile, context)) {
    state?.cancel?.abort();
    const profile = readProfile(context);
    const active = profile.routes.find((route) => route.endpoint === profile.lastEndpoint) ?? context.primary;
    state = { context, profile, active, failed: !sameRoute(active, context.primary), revision: 0, retryAt: 0 };
    states.set(context.backend, state);
  }
  return state;
}

function save(state: State): void {
  if (!state.context.current() || states.get(state.context.backend) !== state) throw new Error('Computer pairing changed. Reconnect to continue.');
  const raw = JSON.stringify(state.profile);
  const key = KEY + encodeURIComponent(state.context.backend);
  localStorage.setItem(key, raw);
  if (localStorage.getItem(key) !== raw) throw new Error('Could not save this computer’s connection addresses.');
}

function assertCurrent(state: State): void {
  if (!state.context.current() || states.get(state.context.backend) !== state) throw new Error('Computer pairing changed. Reconnect to continue.');
}

async function choose(state: State): Promise<ComputerRoute> {
  assertCurrent(state);
  if (!state.failed) return state.active;
  if (state.profile.routes.length === 0) {
    // Older hosts have no alternate-route contract. Keep their ordinary
    // reconnect path; a previous socket failure cannot latch it closed.
    state.failed = false;
    return state.active;
  }
  if (state.selection) return state.selection;
  if (Date.now() < state.retryAt) throw new Error('No verified address for this computer is reachable.');
  const controller = new AbortController();
  state.cancel = controller;
  const timer = setTimeout(() => controller.abort(), 2000);
  const revision = state.revision;
  const routes = [...state.profile.routes];
  if (!routes.some((route) => route.endpoint === state.context.primary.endpoint)) routes.push(state.context.primary);
  const previous = state.active;
  state.selection = (async () => {
    // Prefer another working route after a failure. Keep a successful probe
    // of the old route as a fallback if the alternatives all fail.
    let fallback: ComputerRoute | undefined;
    const alternative = routes.filter((route) => !sameRoute(route, previous));
    const check = async (route: ComputerRoute) => {
      await verifyComputerRoute(route, state.context.backendId, controller.signal);
      return route;
    };
    const old = routes.find((route) => sameRoute(route, previous));
    const previousProbe = old ? check(old).then((route) => { fallback = route; }).catch(() => undefined) : Promise.resolve();
    let selected: ComputerRoute | undefined;
    try { selected = await Promise.any(alternative.map(check)); }
    catch { await previousProbe; selected = fallback; }
    assertCurrent(state);
    if (state.revision !== revision) throw new Error('Computer addresses changed while checking the connection. Retry.');
    if (!selected) throw new Error('No verified address for this computer is reachable.');
    state.active = selected;
    state.failed = false;
    state.profile.lastEndpoint = selected.endpoint;
    // A full storage device does not invalidate a verified, already trusted
    // route. Its saved candidates survive; only the last-working hint is lost.
    try { save(state); } catch { /* The next launch can select again. */ }
    return selected;
  })().catch((error) => { state.retryAt = Date.now() + 250; throw error; }).finally(() => {
    clearTimeout(timer);
    controller.abort();
    state.selection = undefined;
    state.cancel = undefined;
  });
  return state.selection;
}

function waitForRoute(state: State, signal?: AbortSignal | null): Promise<ComputerRoute> {
  if (signal?.aborted) return Promise.reject(signal.reason);
  const selection = choose(state);
  if (!signal) return selection;
  return new Promise((resolve, reject) => {
    const aborted = () => reject(signal.reason);
    signal.addEventListener('abort', aborted, { once: true });
    selection.then(resolve, reject).finally(() => signal.removeEventListener('abort', aborted));
  });
}

/** Called only for a paired computer, never for invitation redemption or an
 * external URL. A failed request is surfaced once; the next one selects. */
export async function fetchComputerRoute(context: ComputerRouteContext, fetcher: typeof fetch, input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  if (!context.primary.endpoint.startsWith('https://') || !await canVerifyComputerRoutes()) {
    context.beforeRequest?.();
    return fetcher(input, init);
  }
  const url = new URL(input instanceof Request ? input.url : String(input), globalThis.location?.href);
  if (url.origin !== context.primary.endpoint || url.username || url.password) throw new Error('Request does not belong to this computer.');
  const state = stateFor(context);
  const signal = init?.signal ?? (input instanceof Request ? input.signal : undefined);
  const selected = await waitForRoute(state, signal);
  assertCurrent(state);
  if (!context.current()) throw new Error('Computer pairing changed. Reconnect to continue.');
  signal?.throwIfAborted();
  context.beforeRequest?.();
  const target = new URL(selected.endpoint);
  url.protocol = target.protocol; url.host = target.host;
  const redirected = input instanceof Request ? new Request(url, input) : url.href;
  try {
    const response = sameRoute(selected, context.primary) ? await fetcher(redirected, init)
      : selected.certFingerprint ? await pinnedFetch(redirected, init, selected.certFingerprint)
      : await globalThis.fetch(redirected, init);
    responseURLs.set(response, url.href);
    if (response.status >= 500 && sameRoute(state.active, selected)) state.failed = true;
    return response;
  } catch (error) {
    if (!signal?.aborted && sameRoute(state.active, selected)) state.failed = true;
    throw error;
  }
}

/** The actual response origin is captured before another request can switch
 * routes. Bootstrap checks its socket URL against this same snapshot. */
export function computerResponseURL(response: Response, requested: string): string { return responseURLs.get(response) ?? requested; }

export async function learnComputerRoutes(context: ComputerRouteContext, advertised: unknown): Promise<void> {
  if (!context.primary.endpoint.startsWith('https://') || !await canVerifyComputerRoutes()) return;
  if (!context.current()) return;
  const state = stateFor(context);
  const routes = mergeComputerRoutes(state.profile.routes, advertised);
  if (JSON.stringify(routes) === JSON.stringify(state.profile.routes)) return;
  state.profile = { ...state.profile, routes };
  // These addresses came from an authenticated manifest. A full storage
  // device may lose the hint on reopening, but cannot block this connection.
  try { save(state); } catch { /* Keep trusted candidates for this page. */ }
  state.revision++;
  state.retryAt = 0;
  const updated = routes.find((route) => route.endpoint === state.active.endpoint);
  if (updated && !sameRoute(updated, state.active)) state.failed = true;
}

/** Bootstrap already selected and verified the route. Socket creation stays
 * synchronous and keeps the same session ticket, path and query. */
export function computerSocketRoute(context: ComputerRouteContext, input: string): { url: string; pin?: string } | null {
  const state = states.get(context.backend);
  if (!state || !bound(state.profile, context)) return null;
  assertCurrent(state);
  if (state.failed) throw new Error('This computer’s address needs verification. Reconnect to continue.');
  const url = new URL(input);
  if (url.username || url.password || url.hash || url.protocol !== 'wss:') throw new Error('Invalid computer socket address.');
  const origin = url.origin.replace(/^wss:/, 'https:');
  if (origin !== context.primary.endpoint && !state.profile.routes.some((route) => route.endpoint === origin)) throw new Error('Socket does not belong to this computer.');
  const target = new URL(state.active.endpoint);
  url.protocol = 'wss:'; url.host = target.host;
  return { url: url.href, pin: state.active.certFingerprint };
}

export function failComputerRoute(context: ComputerRouteContext, input: string): void {
  const state = states.get(context.backend);
  if (!state || !bound(state.profile, context) || !context.current()) return;
  const origin = new URL(input).origin.replace(/^wss:/, 'https:');
  if (origin === state.active.endpoint) state.failed = true;
}

export function forgetComputerRoutes(backend: BackendKey): void {
  states.get(backend)?.cancel?.abort();
  states.delete(backend);
  try { localStorage.removeItem(KEY + encodeURIComponent(backend)); } catch { /* Pairing removal still proceeds. */ }
}

/** Unlike passive advertisements, a user-requested address repair only
 * succeeds after the verified address has been saved for the next launch. */
export async function repairComputerAddress(context: ComputerRouteContext, endpoint: string): Promise<string> {
  if (!await canVerifyComputerRoutes()) throw new Error('Install the latest Android APK to verify a new computer address.');
  const state = stateFor(context);
  assertCurrent(state);
  const candidates = repairComputerRouteCandidates(context.primary, state.profile.routes, endpoint);
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 2000);
  let verified: ComputerRoute | undefined;
  try {
    for (const candidate of candidates) {
      try {
        await verifyComputerRoute(candidate, context.backendId, controller.signal);
        verified = candidate;
        break;
      } catch {
        if (controller.signal.aborted) break;
      }
    }
  } finally { clearTimeout(timer); controller.abort(); }
  if (!verified) throw new Error('Could not verify this computer at that address. Check that it is running and reachable.');
  assertCurrent(state);
  const allowed = repairComputerRouteCandidates(context.primary, state.profile.routes, endpoint);
  if (!allowed.some((candidate) => sameRoute(candidate, verified!))) throw new Error('Computer certificate trust changed while checking. Retry.');
  const previous = state.profile;
  state.profile = { ...previous, routes: mergeComputerRoutes(previous.routes, [verified]), lastEndpoint: verified.endpoint };
  try { save(state); } catch (error) { state.profile = previous; throw error; }
  state.revision++;
  state.active = verified;
  state.failed = false;
  state.retryAt = 0;
  state.cancel?.abort();
  return verified.endpoint;
}
