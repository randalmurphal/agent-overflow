// The backends this client is attached to, one `TransportHandle` each.
//
// Phase 7 of the remote-access spec (§10, "One seam, two realizations")
// keeps ONE resolution point — ./handle.ts — and grows what sits behind
// it from a single connection into a registry. Everything a connection
// owns stays per socket and unchanged: its hello, its session, its replay
// cursors, its watch set, its status. What changes is that there can be
// more than one of them.
//
// **The home entry wraps the `wsClient` singleton rather than replacing
// it.** The page's own backend is the one this document was served by, it
// is the one every existing import of that singleton means, and its
// behaviour has to stay identical to the day before this file existed. So
// it is registered here as an ordinary entry over the existing client,
// and `wsClient` stays exported for the transition.
//
// **The list's SOURCE is one injectable function.** Today it is the
// bootstrap manifest's `backends` array: backends the local Go process
// proxies at same-origin paths (`/ws/backend/<id>` + `/bootstrap/<id>.json`,
// the `clientmode` proxy of spec §10). On a phone the same array comes
// from client-local storage with remote `wss://` URLs and no proxy in
// sight. Neither of those facts belongs in the registry, so the registry
// asks a function for the list and `setBackendSource` is how the phone
// wave supplies a different one — not a second attach path.
//
// **Attachment is EAGER, not lazy.** The unified sidebar's list calls fan
// out over every attached backend at boot (the `all` route below), so a
// lazily-connecting entry would be connected by the first thing the app
// does anyway — one round trip later, and with a "which backend was slow"
// failure mode nobody can read. Eager also keeps the rule that a
// backend's connection depends on NOTHING about visibility, focus, or
// pane position: it is attached or it is not.
//
// Cost: `backendById` is one Map lookup and no allocation; a client with
// one backend never enters the fan-out path at all (./runtime.ts
// dispatches straight to the home handle), so it pays what it paid before.

import type { EventOrigin, TransportHandle } from './handle';
import { HOME_BACKEND, type BackendKey } from './backendKey';
import {
  forgetBackendIdentity,
  getBackendIdentity,
  onBackendIdentity,
} from './backendIdentity';
import { forgetBackendEntities } from './entityIndex';
import { grantedScopes, refreshGrantedScopes, type ScopeSnapshot } from './scopes';
import {
  fetchBackendManifest,
  manifestBackendDescriptors,
  onManifestBackendsChanged,
} from './manifestBackends';
import {
  WSClient,
  wsClient,
  type StepUpProver,
  type TransportStatusSnapshot,
} from './wsClient';

// The registry id of the page's own backend lives in ./backendKey.ts, a
// leaf: several modules this file imports need it too, so it cannot be
// declared here. Re-exported so the registry still reads as its owner.
export { HOME_BACKEND } from './backendKey';
export type { BackendKey } from './backendKey';

/** One entry of the manifest's (or the phone's) attached-backend list. */
export interface BackendDescriptor {
  /** Registry id. Stable across launches; the proxy path segment. */
  id: string;
  /** The backend's own UUID, as pairing recorded it. */
  backendId: string;
  /** Display name — the backend's `backendName`, or the pairing nickname. */
  name: string;
  /** Where this client's socket goes. Same-origin under the desktop
   *  proxy, an absolute `wss://` on a phone. */
  wsUrl: string;
  /** Where its manifest is fetched from. Same rule as `wsUrl`. */
  bootstrapUrl: string;
}

/** A backend this client holds a connection to. */
export interface BackendEntry {
  /** Registry id. `HOME_BACKEND` for the page's own backend. */
  readonly id: string;
  /** True for the page's own backend, false for every attached one. */
  readonly home: boolean;
  /** This backend's socket. One `WSClient` per backend, never shared. */
  readonly client: WSClient;
  /** The door RPCs and subscriptions for this backend go through. */
  readonly handle: TransportHandle;
  /** Display name, as the descriptor supplied it. Empty for home, whose
   *  name is the hello frame's `backendName` and belongs to the UI wave. */
  readonly name: string;
  /** Live history identity, `''` until this backend's manifest resolves. */
  readonly backendId: string;
  readonly generation: string;
  /** What this backend's session was granted. Per backend, because a
   *  device can be full-access on one machine and view-only on another. */
  readonly scopes: ScopeSnapshot;
  /** This connection's own status. The runes mirror the UI renders from is
   *  `stores/transportStatus.svelte.ts`, keyed by the same id. */
  readonly status: TransportStatusSnapshot;
  /**
   * The last `all`-route share this backend failed to supply, or null. A
   * fan-out never rejects as a whole for one backend's failure — the share
   * is dropped and recorded here — so this is the only place that loss is
   * visible.
   */
  readonly lastFanoutError: unknown;
}

// A registry entry: the public view plus what only this module writes.
interface Entry extends BackendEntry {
  /** Cached origin object, rebuilt only when this backend's identity
   *  moves. Events stamp it by reference; a fresh object per frame would
   *  be pure garbage on the busiest path in the app. */
  origin: EventOrigin;
  lastFanoutError: unknown;
}

// Attach order, home first. An array rather than the Map's iteration
// order because the fan-out walks it on every `all` call, and an array
// walk is the cheaper of the two.
const entries: Entry[] = [];
// Every id an entry answers to: its registry id, and its live backend UUID
// once a manifest has named one. Two keys onto one object, so
// `backendById` stays a single lookup whether the caller holds the
// registry id or the UUID off an event's origin stamp.
const byId = new Map<string, Entry>();

const changeListeners = new Set<() => void>();

// Subscriptions that must exist on EVERY attached backend, so a backend
// attached later gets them too. ./runtime.ts's event fan-out is the one
// consumer; see `subscribeEveryBackend`.
interface StandingSubscription {
  channel: string;
  handler: (data: unknown, handle: TransportHandle) => void;
  cancels: Map<Entry, () => void>;
}
const standing = new Set<StandingSubscription>();

// The step-up prover, installed on every handle and on every later attach.
// Held here rather than in ./stepUp.ts because "every attached handle" is
// a fact this module owns and that module must not have to know
// (transport/AGENTS.md: there is one interception, not one per connection
// somebody remembers to wire).
let installedProver: StepUpProver | null = null;

// The handle for one entry. Identical in shape to the single-connection
// handle it replaced.
function createHandle(entry: () => Entry, id: string): TransportHandle {
  return {
    get origin(): EventOrigin {
      const self = entry();
      const backendId = getBackendIdentity(id).backendId;
      if (self.origin.backendId !== backendId) self.origin = { backendId };
      return self.origin;
    },
    callByID(methodId: number, args: unknown[]): Promise<unknown> {
      return entry().client.callByID(methodId, args);
    },
    callByName(method: string, args: unknown[]): Promise<unknown> {
      return entry().client.callByName(method, args);
    },
    installStepUpProver(prover: StepUpProver | null): void {
      entry().client.installStepUpProver(prover);
    },
    subscribe(channel: string, handler: (data: unknown) => void): () => void {
      return entry().client.subscribe(channel, handler);
    },
  };
}

// The home entry's client, held in a variable rather than captured, so a
// suite can stage the page's own connection without a socket
// (`__setHomeClientForTest`). Production writes it exactly once, here.
let homeClient: WSClient = wsClient;

function makeEntry(
  id: string,
  home: boolean,
  client: WSClient,
  name: string,
): Entry {
  let handle: TransportHandle;
  const entry: Entry = {
    id,
    home,
    name,
    get client(): WSClient {
      return home ? homeClient : client;
    },
    origin: { backendId: '' },
    lastFanoutError: null,
    get handle(): TransportHandle {
      return handle;
    },
    get backendId(): string {
      return getBackendIdentity(id).backendId;
    },
    get generation(): string {
      return getBackendIdentity(id).generation;
    },
    get scopes(): ScopeSnapshot {
      return grantedScopes(id);
    },
    get status(): TransportStatusSnapshot {
      return entry.client.getStatus();
    },
  };
  handle = createHandle(() => entry, id);
  return entry;
}

// The page's own backend, registered at module load over the existing
// singleton. Nothing is CALLED on the client here: construction must not
// open a socket, because the boot sequence decides when that happens.
const homeEntry = makeEntry(HOME_BACKEND, true, wsClient, '');
entries.push(homeEntry);
byId.set(HOME_BACKEND, homeEntry);

// A backend's live UUID becomes a second key onto its entry the moment its
// manifest names one, so the origin stamp on a hot event path resolves by
// lookup rather than by scan. Subscribed rather than called from
// ./backendIdentity.ts, which must not import this module: identity is a
// fact about a connection, and the registry is a fact about the app.
onBackendIdentity((identity, backendKey) => {
  if (identity.backendId === '') return;
  const entry = byId.get(backendKey);
  if (entry === undefined) return;
  const existing = byId.get(identity.backendId);
  // Never take over a key another entry holds: two backends claiming one UUID
  // is a backend bug, and resolving it by last-writer-wins would move
  // somebody's threads to the wrong machine.
  if (existing === undefined) byId.set(identity.backendId, entry);
});

/** The page's own backend. Always attached; never detachable. */
export function homeBackend(): BackendEntry {
  return homeEntry;
}

/**
 * Every attached backend, home first, in attach order.
 *
 * A plain array, deliberately not reactive: this is the transport-level
 * view, walked on the RPC fan-out path. The reactive mirror the UI reads
 * is `stores/attachedBackends.svelte.ts`.
 */
export function attachedBackends(): readonly BackendEntry[] {
  return entries;
}

/** How many backends are attached. One means every route resolves home. */
export function attachedBackendCount(): number {
  return entries.length;
}

/**
 * The backend registered under `id`, resolving BOTH spellings: the
 * registry id and the backend's live UUID (which is what an event's origin
 * stamp carries). `undefined` when nothing answers to it — callers that
 * must have an answer fall back to home rather than throwing, since a
 * single-backend app has to behave exactly as it did.
 */
export function backendById(id: string): BackendEntry | undefined {
  return byId.get(id);
}

/**
 * The REGISTRY key for an event origin's backend UUID.
 *
 * An origin stamp carries the UUID (`{backendId}` on every delivered
 * event); everything keyed per backend on this side is keyed by registry
 * id, because that key exists before the first manifest resolves and the
 * UUID does not. This is the one translation between them.
 *
 * An unknown or empty stamp answers HOME. Every event on a single-backend
 * client arrives before its own manifest has resolved at least once — the
 * hello frame precedes it — so "unstamped" has always meant the only
 * connection there is, and reading it as anything else would key a
 * single-backend app's state under a backend it is not attached to.
 */
export function backendKeyForOrigin(backendId: string): BackendKey {
  if (backendId === '') return HOME_BACKEND;
  return byId.get(backendId)?.id ?? HOME_BACKEND;
}

/**
 * Attach a backend. Idempotent on the registry id: re-attaching an id
 * already held answers the existing entry rather than opening a second
 * socket to the same machine.
 *
 * Connects EAGERLY — the standing subscriptions are installed here, and a
 * subscription is what opens a `WSClient`'s socket.
 */
export function attachBackend(descriptor: BackendDescriptor): BackendEntry {
  if (descriptor.id === HOME_BACKEND) return homeEntry;
  const held = byId.get(descriptor.id);
  if (held !== undefined) return held;
  const entry = makeEntry(
    descriptor.id,
    false,
    new WSClient({ bootstrap: () => fetchBackendManifest(descriptor) }),
    descriptor.name,
  );
  entries.push(entry);
  byId.set(descriptor.id, entry);
  if (descriptor.backendId !== '' && !byId.has(descriptor.backendId)) {
    byId.set(descriptor.backendId, entry);
  }
  if (installedProver !== null) entry.handle.installStepUpProver(installedProver);
  // Resolve this backend's grant set from the credential stored for it,
  // BEFORE anything renders against it. A surface that mounted on the
  // unresolved snapshot would hide every control the backend would in fact
  // have allowed, and nothing would invalidate it until the next reconnect.
  refreshGrantedScopes(entry.id);
  for (const sub of standing) attachStanding(sub, entry);
  notifyBackendsChanged();
  return entry;
}

/**
 * Detach a backend and close its socket. The home backend is never
 * detachable — it is the page's own connection, and a page with no
 * connection has nothing to be.
 */
export function detachBackend(id: string): void {
  const entry = byId.get(id);
  if (entry === undefined || entry.home) return;
  const at = entries.indexOf(entry);
  if (at >= 0) entries.splice(at, 1);
  for (const [key, held] of byId) {
    if (held === entry) byId.delete(key);
  }
  for (const sub of standing) {
    sub.cancels.get(entry)?.();
    sub.cancels.delete(entry);
  }
  entry.client.close();
  // Everything keyed on this backend goes with it. Leaving the entity
  // index populated would resolve a thread to a machine this client is no
  // longer attached to, and `resolveTransport` would silently answer home
  // — the same row, the wrong backend, with nothing to see it happen.
  forgetBackendEntities(entry.id);
  forgetBackendIdentity(entry.id);
  notifyBackendsChanged();
}

/** Subscribe to attach/detach. Does NOT fire immediately; callers read
 *  `attachedBackends()` for the current list. */
export function onBackendsChanged(listener: () => void): () => void {
  changeListeners.add(listener);
  return () => {
    changeListeners.delete(listener);
  };
}

function notifyBackendsChanged(): void {
  for (const listener of changeListeners) listener();
}

// ---------------------------------------------------------------------------
// Standing subscriptions
// ---------------------------------------------------------------------------

function attachStanding(sub: StandingSubscription, entry: Entry): void {
  const handle = entry.handle;
  sub.cancels.set(
    entry,
    handle.subscribe(sub.channel, (data) => {
      sub.handler(data, handle);
    }),
  );
}

/**
 * Subscribe to `channel` on EVERY attached backend, now and later.
 *
 * The handler receives the DELIVERING handle, so the origin stamp comes
 * from the connection the frame actually arrived on rather than from
 * whichever backend happened to be resolvable at subscribe time. That is
 * the whole difference between one backend and several: the stamp used to
 * be a property of the app and is now a property of the delivery.
 *
 * One consumer: ./runtime.ts's `Events.On`.
 */
export function subscribeEveryBackend(
  channel: string,
  handler: (data: unknown, handle: TransportHandle) => void,
): () => void {
  const sub: StandingSubscription = { channel, handler, cancels: new Map() };
  standing.add(sub);
  for (const entry of entries) attachStanding(sub, entry);
  return () => {
    if (!standing.delete(sub)) return;
    for (const cancel of sub.cancels.values()) cancel();
    sub.cancels.clear();
  };
}

/**
 * Install the step-up prover on every attached handle, and on every
 * backend attached afterwards.
 *
 * ./stepUp.ts calls this once at boot. The "and afterwards" half is the
 * point: a per-handle install done at boot alone would leave a backend
 * attached from Settings unable to satisfy a step-up refusal, and the
 * omission would be invisible on the owner's own machine — the exact
 * recurring shape transport/AGENTS.md describes for this seam.
 */
export function installStepUpProverEverywhere(prover: StepUpProver | null): void {
  installedProver = prover;
  for (const entry of entries) entry.handle.installStepUpProver(prover);
}

// ---------------------------------------------------------------------------
// The `all` route: fan out and merge
// ---------------------------------------------------------------------------

/**
 * THE MERGE RULE, stated once and implemented once.
 *
 * An `all`-routed call is asked of every attached backend and the answers
 * are combined by SHAPE, because the wire carries no envelope to name the
 * backend an answer came from and adding one would change every list
 * method's type:
 *
 *  - Every share an ARRAY → concatenated in attach order (home first).
 *    Order within a backend's share is preserved; cross-backend order is
 *    not meaningful (the sidebar sorts by activity) and is not invented.
 *  - Every share a plain OBJECT (an id-keyed record — `GetUIState`'s
 *    shape) → shallow-merged, later backends winning a key collision. Ids
 *    are globally unique (spec §10, `internal/entityid`), so a collision
 *    is a bug elsewhere rather than a case to resolve here.
 *  - Anything else — a scalar, a mixed set → the HOME backend's share
 *    verbatim. There is no way to combine two numbers into one that means
 *    anything, and inventing one would be worse than naming the machine
 *    whose answer the caller gets.
 *
 * `null` and `undefined` shares are dropped before the shape is judged: a
 * backend that legitimately answers "nothing" must not demote the merge to
 * the scalar arm.
 */
export function mergeBackendResults(shares: readonly unknown[], homeShare: unknown): unknown {
  const present: unknown[] = [];
  for (const share of shares) {
    if (share !== null && share !== undefined) present.push(share);
  }
  if (present.length === 0) return homeShare;
  if (present.length === 1) return present[0];
  let allArrays = true;
  let allObjects = true;
  for (const share of present) {
    if (Array.isArray(share)) allObjects = false;
    else if (typeof share === 'object') allArrays = false;
    else return homeShare;
  }
  if (allArrays) {
    const out: unknown[] = [];
    for (const share of present) out.push(...(share as unknown[]));
    return out;
  }
  if (allObjects) return Object.assign({}, ...(present as object[])) as unknown;
  return homeShare;
}

/**
 * Dispatch an `all`-routed call to every attached backend and merge.
 *
 * A backend that fails supplies no share: the failure is recorded on its
 * entry (`lastFanoutError`) and the merge proceeds, because one
 * unreachable machine must not blank the sidebar of the ones that are
 * reachable. The whole call rejects only when EVERY backend failed, and
 * then with the home backend's own error — which is what the
 * single-backend app has always done, and what the toast beside it says.
 *
 * `observe` is handed each backend's own share before the merge; it is how
 * ./entityIndex.ts learns which machine a row came from, which the merged
 * value can no longer say.
 */
export async function callEveryBackend(
  methodId: number,
  args: unknown[],
  observe?: (result: unknown, backendId: string) => void,
): Promise<unknown> {
  const targets = entries.slice();
  const settled = await Promise.allSettled(
    targets.map((entry) => entry.client.callByID(methodId, args)),
  );
  const shares: unknown[] = [];
  let homeShare: unknown;
  let homeError: unknown = null;
  let firstError: unknown = null;
  let anyFulfilled = false;
  for (let i = 0; i < targets.length; i += 1) {
    const entry = targets[i];
    const outcome = settled[i];
    if (outcome.status === 'fulfilled') {
      anyFulfilled = true;
      entry.lastFanoutError = null;
      shares.push(outcome.value);
      observe?.(outcome.value, entry.id);
      if (entry.home) homeShare = outcome.value;
      continue;
    }
    entry.lastFanoutError = outcome.reason;
    if (firstError === null) firstError = outcome.reason;
    if (entry.home) homeError = outcome.reason;
  }
  if (!anyFulfilled) throw homeError ?? firstError ?? new Error('no backend answered');
  return mergeBackendResults(shares, homeShare);
}

// ---------------------------------------------------------------------------
// Naming a backend for one call
// ---------------------------------------------------------------------------

// The backend the NEXT dispatched call routes to, and no other.
//
// Same shape as `wsClient.withStepUpToken` and for the same reason: it is
// armed for the SYNCHRONOUS span of the callback and drained at dispatch,
// because a slot left standing across an await would put somebody else's
// call on this machine. It exists for the one class of method whose route
// is per connection rather than per entity — `appStorage`'s ui_state
// bucket, which belongs to the backend that stores it — and it is not a
// general-purpose override: a method that always wants a different backend
// wants a different ROUTE.
let pinnedTarget: BackendKey | null = null;

/**
 * Route the ONE call `issue` dispatches to `backendId`.
 *
 * `issue` must dispatch exactly one RPC, synchronously. Whatever it
 * returns is handed back untouched, so the ordinary
 * `await withBackendTarget(id, () => SomeMethod())` reads as a normal
 * call.
 */
export function withBackendTarget<T>(backendId: BackendKey, issue: () => T): T {
  pinnedTarget = backendId;
  try {
    return issue();
  } finally {
    pinnedTarget = null;
  }
}

/** Take the pinned target, if one was armed. Drained at dispatch. */
export function takePinnedBackend(): BackendKey | null {
  const target = pinnedTarget;
  pinnedTarget = null;
  return target;
}

// ---------------------------------------------------------------------------
// Where the list comes from
// ---------------------------------------------------------------------------

/**
 * The source of the attached-backend list. ONE function, replaced rather
 * than branched on.
 *
 * The default answers nothing, and ./bootstrap.ts installs the manifest
 * reader at boot: the desktop's local process publishes the backends it
 * proxies in the manifest's `backends` array. A phone shell replaces this
 * with a reader over client-local storage whose entries carry remote
 * `wss://` URLs. Nothing else about attachment differs between the two,
 * which is why there is one seam here and no client-class branch below it.
 */
export type BackendSource = () => readonly BackendDescriptor[];

// The default source is what the bootstrap manifest published
// (./manifestBackends.ts, a leaf so `bootstrap → backends` never closes a
// ring around the `wsClient` singleton). Re-swept whenever that list moves,
// which is when somebody adds or removes a machine.
let backendSource: BackendSource = manifestBackendDescriptors;

onManifestBackendsChanged(() => {
  syncAttachedBackends();
});

export function setBackendSource(source: BackendSource): void {
  backendSource = source;
}

/**
 * Attach everything the source names, and detach anything held that it no
 * longer does. Idempotent; safe to call again when the source changes.
 */
export function syncAttachedBackends(): void {
  const wanted = backendSource();
  const keep = new Set<string>([HOME_BACKEND]);
  for (const descriptor of wanted) {
    if (typeof descriptor?.id !== 'string' || descriptor.id === HOME_BACKEND) continue;
    keep.add(descriptor.id);
    attachBackend(descriptor);
  }
  for (const entry of entries.slice()) {
    if (!keep.has(entry.id)) detachBackend(entry.id);
  }
}

/**
 * Test seam: stage the page's own connection.
 *
 * The home entry normally wraps the `wsClient` singleton, which a suite
 * cannot replace by mocking that module alone — `src/test/setup.ts` loads
 * the real one before any test file's `vi.mock` registers. This is how a
 * transport test points the home handle at its own fake.
 */
export function __setHomeClientForTest(client: WSClient): void {
  homeClient = client;
}

/** Test seam: drop every attached backend, leaving only home. */
export function __resetBackendsForTest(): void {
  for (const entry of entries.slice()) {
    if (!entry.home) detachBackend(entry.id);
  }
  homeEntry.lastFanoutError = null;
  installedProver = null;
  backendSource = manifestBackendDescriptors;
}

/**
 * Test seam: attach a backend over a caller-supplied client, so a suite
 * can stage two connections without a socket or a manifest fetch. The one
 * thing production attach does that this skips is constructing the client.
 */
export function __attachBackendForTest(
  descriptor: BackendDescriptor,
  client: WSClient,
): BackendEntry {
  const entry = makeEntry(descriptor.id, false, client, descriptor.name);
  entries.push(entry);
  byId.set(descriptor.id, entry);
  if (descriptor.backendId !== '' && !byId.has(descriptor.backendId)) {
    byId.set(descriptor.backendId, entry);
  }
  if (installedProver !== null) entry.handle.installStepUpProver(installedProver);
  refreshGrantedScopes(entry.id);
  for (const sub of standing) attachStanding(sub, entry);
  notifyBackendsChanged();
  return entry;
}
