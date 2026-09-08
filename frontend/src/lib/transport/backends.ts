// The backends this client is attached to, one `TransportHandle` each.
//
// Phase 7 of the remote-access spec (§10, "One seam, two realizations")
// keeps ONE resolution point — ./handle.ts — and grows what sits behind
// it from a single connection into a registry. Everything a connection
// owns stays per socket and unchanged: its hello, its session, its replay
// cursors, its watch set, its status. What changes is that there can be
// more than one of them.
//
// **The page's own backend is an ordinary entry.** Its descriptor carries
// the registry id `HOME_BACKEND` and comes from the same source as every
// other backend's (./manifestBackends.ts, `defaultBackendDescriptors`).
// The one thing `attachBackend` does differently for it is hand it the
// `wsClient` singleton as its client instead of constructing one, because
// that singleton is what every remaining external import of it means and
// its behaviour has to stay identical to the day before this file
// existed. Attach, detach, the `all` fan-out, the standing subscriptions
// and every "everywhere" fan-out treat it exactly as they treat a machine
// attached from Settings: there is one wiring path, and home goes through
// it.
//
// **The list's SOURCE is one function, and it decides the client class
// once.** On a desktop it answers home plus the bootstrap manifest's
// `backends` array: backends the local Go process proxies at same-origin
// paths (`/ws/backend/<id>` + `/bootstrap/<id>.json`, the `clientmode`
// proxy of spec §10). On a phone it answers the endpoint map the shell
// persisted — home included only while its legacy slot is stored — with
// remote `wss://` URLs and no proxy in sight. Neither fact belongs in the
// registry, which only ever asks the source and reconciles against it: at
// module load, whenever the manifest's list moves, on every native boot,
// and when a shell's home learns which computer it is.
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
// one backend never fans out (`callEveryBackend` dispatches its single
// target directly), so it pays what it paid before.

import { rememberedIdentity, forgetRememberedIdentity } from './rememberedIdentity';
import { isNativeShell } from '../native/platform';
import { isFrontendOnly } from './runMode';
import { storedBackendEndpoint } from './homeEndpoint';
import { pairedComputerId } from './deviceSession';
import type { LeaseState } from './frames';
import type { EventOrigin, TransportHandle } from './handle';
import { HOME_BACKEND, type BackendKey } from './backendKey';
import {
  forgetBackendIdentity,
  getBackendIdentity,
  onBackendIdentity,
} from './backendIdentity';
import { captureThreadMetadataRead, forgetBackendEntities, onThreadOwnershipChanged, threadBackend, type ForgottenEntities } from './entityIndex';
import { forgetBackendClock, registerBackendClock } from './backendClock';
import { grantedScopes, refreshGrantedScopes, forgetGrantedScopes, type ScopeSnapshot } from './scopes';
import {
  __resetManifestBackendsForTest,
  defaultBackendDescriptors,
  fetchBackendManifest,
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
  /** Explicit desktop profile override; absent on legacy combined-name manifests. */
  nickname?: string;
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
  readonly nickname?: string;
  /** Stable computer identity, available from pairing even while offline. */
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
  name: string;
  /** The addressing snapshot this backend was attached under; replaced
   *  whole when the source names the same id with new fields. */
  descriptor: BackendDescriptor;
  /** The UUID a descriptor named for this backend. A later snapshot may
   *  name one an earlier did not (a manifest resolving); none blanks it. */
  pairedBackendId: string;
  /** Writable for one reason: a suite re-points a held entry at a fake
   *  (`__attachBackendForTest`). Production never reassigns it. */
  client: WSClient;
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
// The frontend-only controller still receives local administrative calls and
// events. It has no projects, history, accounts or execution scope to fan out to.
const computers: Entry[] = isFrontendOnly() ? [] : entries;
// Every id an entry answers to: its registry id, and its live backend UUID
// once a manifest has named one. Two keys onto one object, so
// `backendById` stays a single lookup whether the caller holds the
// registry id or the UUID off an event's origin stamp.
const byId = new Map<string, Entry>();

const changeListeners = new Set<() => void>();

/**
 * What a detach took with it: the backend, and every row id this client
 * held for it.
 *
 * The ids travel IN the notification rather than being looked up by the
 * listener, and that is the whole design. A row store has to drop exactly
 * the rows whose backend just left, and the only place that set exists is
 * the entity index the same call clears. A listener that asked the index
 * would have to be sequenced BEFORE the clear, and an ordering that has to
 * be remembered is one that is right on the machine it was written on and
 * wrong on the next. Carrying the payload removes the ordering entirely.
 */
export interface BackendDetachment {
  /** The registry id that is no longer attached. */
  readonly backendId: BackendKey;
  /** Thread ids this client had indexed to it. */
  readonly threadIds: readonly string[];
  /** Project ids this client had indexed to it. */
  readonly projectIds: readonly string[];
  /** Thread-group ids this client had indexed to it. */
  readonly threadGroupIds: readonly string[];
  /** Workflow items whose detail, receipts and requests must be released. */
  readonly workflowItemIds: readonly string[];
}

const detachListeners = new Set<(detached: BackendDetachment) => void>();

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
type DiagnosticsSink = (message: string, detail?: string) => void;
let installedDiagnosticsSink: DiagnosticsSink | null = null;

function installEntryDiagnostics(entry: Entry): void {
  entry.client.setDiagnosticsSink(installedDiagnosticsSink === null ? null : (message, detail = '') => {
    installedDiagnosticsSink?.(message, `backend=${entry.id || 'home'} ${detail}`);
  });
}

/** One sink covers existing connections and every later attachment. */
export function installDiagnosticsSinkEverywhere(sink: DiagnosticsSink | null): void {
  installedDiagnosticsSink = sink;
  for (const entry of entries) installEntryDiagnostics(entry);
}

// The client's foreground lifecycle, held here for the same reason the prover
// is: "every attached backend, and every one attached afterwards" is a fact
// this module owns. The lifecycle is whole-CLIENT — one OS pausing one app —
// so a second machine's connection is exactly as backgrounded as the first,
// and a backend attached while the phone is asleep must be told so at attach
// rather than at the next resume.
let clientLease: LeaseState = 'active';

// The whole watched-thread set, unsplit. Held for the third instance of the
// same reason: which machines exist is this module's fact, and the composing
// store (stores/watchedThreads.ts) must not have to learn it. Kept whole
// rather than per backend because the split depends on ./entityIndex.ts,
// which moves under it — a thread's owner becomes known after the set that
// carried it was already sent.
let watchedThreadIds: string[] = [];
onThreadOwnershipChanged((id) => {
  if (watchedThreadIds.includes(id)) for (const entry of entries) sendWatchedThreads(entry);
});

// What this screen last said it was doing, held for the fourth instance of
// the same reason: "every attached backend, and every one attached
// afterwards" is this module's fact. `null` is "nothing composed yet", which
// every backend treats as unattended, so a backend attached before the first
// compose needs no catch-up frame.
let screenPresence: { focused: boolean; threads: string[] } | null = null;

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
    setLease(state: LeaseState): void {
      entry().client.setLease(state);
    },
    setWatchedThreads(threadIds: readonly string[]): void {
      entry().client.setWatchedThreads(threadIds);
    },
    setPresence(focused: boolean, threadIds: readonly string[]): void {
      entry().client.setPresence(focused, threadIds);
    },
    subscribe(channel: string, handler: (data: unknown) => void): () => void {
      return entry().client.subscribe(channel, handler);
    },
  };
}

function makeEntry(descriptor: BackendDescriptor, client: WSClient): Entry {
  const id = descriptor.id;
  let handle: TransportHandle;
  const entry: Entry = {
    id,
    home: id === HOME_BACKEND,
    name: descriptor.name,
    descriptor,
    pairedBackendId: descriptor.backendId,
    client,
    get nickname(): string | undefined { return entry.descriptor.nickname; },
    origin: { backendId: '' },
    lastFanoutError: null,
    get handle(): TransportHandle {
      return handle;
    },
    get backendId(): string {
      return getBackendIdentity(id).backendId || entry.pairedBackendId || pairedComputerId(id) || rememberedIdentity(id)?.backendId || '';
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
  if (installedDiagnosticsSink !== null) installEntryDiagnostics(entry);
  // Its clock, for anything formatting a timestamp this backend minted.
  // A closure over the entry rather than a copied number: the reading
  // moves on every reconnect and wsClient does not publish a hello whose
  // only change is the clock (./backendClock.ts says why).
  registerBackendClock(id, () => entry.client.getHello()?.clockSkewMs ?? 0);
  return entry;
}

// Clients a suite staged per registry id, consulted before one would be
// constructed (`__attachBackendForTest`). Empty in production.
const stagedClients = new Map<string, WSClient>();

// The client an entry is attached over. The page's own backend keeps the
// `wsClient` singleton — what every remaining external import of it means,
// and the one thing about home this file does differently (see the
// header). Every other backend gets a connection of its own, bootstrapped
// through the manifest fetcher ./bootstrap.ts installed. Nothing is CALLED
// on the client here: construction must not open a socket, because the
// boot sequence decides when that happens.
function clientFor(id: string, descriptor: () => BackendDescriptor): WSClient {
  const staged = stagedClients.get(id);
  if (staged !== undefined) return staged;
  if (id === HOME_BACKEND) return wsClient;
  return new WSClient({
    bootstrap: () => fetchBackendManifest(descriptor()),
    // Its own credential slot. Empty on a desktop, where the local
    // process holds the profile and proxies same-origin; a phone's
    // slot per machine is what makes its dial name the right session.
    backend: id,
  });
}

// A backend's live UUID becomes a second key onto its entry the moment its
// manifest names one, so the origin stamp on a hot event path resolves by
// lookup rather than by scan. Subscribed rather than called from
// ./backendIdentity.ts, which must not import this module: identity is a
// fact about a connection, and the registry is a fact about the app.
onBackendIdentity((identity, backendKey) => {
  if (identity.backendId === '') return;
  // A shell's stored legacy home slot learns which computer it is only from
  // an authenticated manifest, and the stored source's answer moves with
  // that fact alone: a slot proven to duplicate a UUID pairing retires, and
  // a UUID sibling whose own pairing is incomplete folds into the slot.
  // Reconcile BEFORE choosing which entry owns the UUID alias below — and
  // only for that slot's own identity, so no other backend's pays the sweep.
  if (backendKey === HOME_BACKEND && isNativeShell() && storedBackendEndpoint() !== '') {
    syncAttachedBackends();
  }
  const entry = byId.get(backendKey);
  if (entry === undefined) return;
  const existing = byId.get(identity.backendId);
  // Never take over a key another entry holds: two backends claiming one UUID
  // is a backend bug, and resolving it by last-writer-wins would move
  // somebody's threads to the wrong machine.
  if (existing === undefined) byId.set(identity.backendId, entry);
});

/**
 * Execution computers in attach order, home first when it executes work.
 * The standalone frontend's administrative controller is excluded.
 *
 * A plain array, deliberately not reactive: this is the transport-level
 * view, walked on the RPC fan-out path. The reactive mirror the UI reads
 * is `stores/attachedBackends.svelte.ts`.
 */
export function attachedBackends(): readonly BackendEntry[] {
  return computers;
}

/** How many backends are attached, including a legacy home slot when present. */
export function attachedBackendCount(): number {
  return computers.length;
}

/** An unknown entity is unambiguous only while exactly one computer is attached. */
export function requireEntityBackend(owner: BackendKey | undefined): BackendKey {
  if (owner !== undefined) return owner;
  if (computers.length === 1) return computers[0].id;
  throw new Error('The computer that owns this item is unknown. Refresh it before continuing.');
}

/**
 * The backend registered under `id`, resolving BOTH spellings: the
 * registry id and the backend's live UUID (which is what an event's origin
 * stamp carries). `undefined` when nothing answers to it. An explicit
 * target must never fall back to another computer when it is absent.
 */
/** The immutable addressing snapshot used for this connection's bootstrap. */
export function backendDescriptor(id: BackendKey): BackendDescriptor | undefined {
  return byId.get(id)?.descriptor;
}

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
  const held = byId.get(descriptor.id);
  if (held !== undefined) {
    // A nickname change arrives as a NAME change (`systems.systemLabel`
    // folds it in), and `manifestBackends.sameDescriptors` dedups the
    // publish on the same fields, so the name is the one diff here.
    held.descriptor = descriptor;
    if (descriptor.backendId !== '') {
      held.pairedBackendId = descriptor.backendId;
      if (!byId.has(descriptor.backendId)) byId.set(descriptor.backendId, held);
    }
    if (held.name !== descriptor.name) {
      held.name = descriptor.name;
      notifyBackendsChanged();
    }
    return held;
  }
  const entry = makeEntry(descriptor, clientFor(descriptor.id, () => entry.descriptor));
  // Home first, then attach order. The fan-out concatenates shares and the
  // machine list renders in this order, and a shell re-pairing its legacy
  // home after other machines are up must not move it to the end.
  if (entry.home) entries.unshift(entry);
  else entries.push(entry);
  // The frontend-only controller's home is a local administrative
  // connection, not an execution computer.
  if (computers !== entries && !entry.home) computers.push(entry);
  byId.set(descriptor.id, entry);
  if (descriptor.backendId !== '' && !byId.has(descriptor.backendId)) {
    byId.set(descriptor.backendId, entry);
  }
  if (installedProver !== null) entry.handle.installStepUpProver(installedProver);
  // A backend attached while the client is asleep is told so now, not at
  // the next resume: it would otherwise stream at full rate to a paused app.
  if (clientLease !== 'active') entry.handle.setLease(clientLease);
  // And the watched set, for the same reason: a machine attached while
  // panes are already open would otherwise push nothing for them until
  // the next composition change, which on a settled screen is never.
  if (watchedThreadIds.length > 0) sendWatchedThreads(entry);
  // And this screen's presence, which a backend attached mid-session would
  // otherwise read as unattended until the next focus change — on a settled
  // screen, never.
  sendScreenPresence(entry);
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
 * Detach a backend and close its socket. Home is no exception here. A
 * desktop's is never asked to leave — `backendAttach.detachAttachedBackend`
 * refuses it, and its source always names it, so a sync keeps it — and a
 * phone's legacy first pairing is ordinary remote access that goes when
 * its stored slot does.
 */
export function detachBackend(id: string): void {
  const entry = byId.get(id);
  if (entry === undefined) return;
  const at = entries.indexOf(entry);
  if (at >= 0) entries.splice(at, 1);
  if (computers !== entries) {
    const computerAt = computers.indexOf(entry);
    if (computerAt >= 0) computers.splice(computerAt, 1);
  }
  for (const [key, held] of byId) {
    if (held === entry) byId.delete(key);
  }
  for (const sub of standing) {
    sub.cancels.get(entry)?.();
    sub.cancels.delete(entry);
  }
  entry.client.close();
  if (installedDiagnosticsSink !== null) entry.client.setDiagnosticsSink(null);
  forgetGrantedScopes(entry.id);
  // Everything keyed on this backend goes with it. Leaving the entity
  // index populated would resolve a thread to a machine this client is no
  // longer attached to, and `resolveTransport` would silently answer home:
  // the same row, the wrong backend, with nothing to see it happen.
  //
  // Clearing the index alone is not enough, and the reason is the same
  // sentence read the other way. The ROW STORES still hold that machine's
  // threads, projects and groups, and once the index has forgotten them
  // every call about one resolves home. So the ids that just went are
  // handed to `onBackendDetached` BEFORE anything can render again, and
  // the stores that hold rows drop theirs. Detach is not a place where a
  // send may quietly land on another machine (spec §10: never a silent
  // failover).
  const forgotten: ForgottenEntities = forgetBackendEntities(entry.id);
  forgetBackendIdentity(entry.id);
  // Its clock goes with it. A reading held for a machine nothing is
  // attached to would keep skewing whatever still names that id.
  forgetBackendClock(entry.id);
  notifyBackendDetached({ backendId: entry.id, ...forgotten });
  forgetRememberedIdentity(entry.id);
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

/**
 * Subscribe to a backend LEAVING, with the rows it took.
 *
 * Separate from `onBackendsChanged` because the two answer different
 * questions: that one is "the list moved, re-derive", this one is "these
 * exact ids are gone, drop them". A listener here must not throw; one that
 * does would strand the detach half-done, so the failure is contained and
 * logged and the rest still run.
 */
export function onBackendDetached(
  listener: (detached: BackendDetachment) => void,
): () => void {
  detachListeners.add(listener);
  return () => {
    detachListeners.delete(listener);
  };
}

function notifyBackendDetached(detached: BackendDetachment): void {
  for (const listener of detachListeners) {
    try {
      listener(detached);
    } catch (err) {
      console.warn('transport: a backend-detach listener threw', err);
    }
  }
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

/**
 * State the client's foreground lifecycle on every attached backend, and on
 * every backend attached afterwards.
 *
 * ./lease.ts is the door; this is the fan-out. It is unconditional across
 * backends by construction: the signal is one app being paused by one OS,
 * not a per-connection preference, so there is no shape in which one
 * attached machine is backgrounded and another is not.
 */
export function setLeaseEverywhere(state: LeaseState): void {
  clientLease = state;
  for (const entry of entries) entry.handle.setLease(state);
}

/**
 * State this screen's presence on every attached backend, and on every
 * backend attached afterwards.
 *
 * `stores/screenPresence.ts` is the door; this is the fan-out. REPEATED
 * rather than split, unlike the watch set, and for a reason that only looks
 * like an oversight: thread ids are unique across backends
 * (`internal/entityid`), so a machine that does not hold the thread matches
 * nothing and a machine that does gets the truth. Splitting it would risk
 * withholding an id whose owner this client has not learned yet — and the
 * cost of that is a notification about the very thread the person is reading.
 *
 * Each backend decides for itself whether this screen is ITS screen, from
 * the connection's origin, so a phone stating a focused presence to the
 * desktop it is attached to silences nothing there.
 */
export function setPresenceEverywhere(focused: boolean, threadIds: readonly string[]): void {
  screenPresence = { focused, threads: [...threadIds] };
  for (const entry of entries) sendScreenPresence(entry);
}

function sendScreenPresence(entry: Entry): void {
  if (screenPresence === null) return;
  entry.handle.setPresence(screenPresence.focused, screenPresence.threads);
}

/**
 * State the watched-thread set on every attached backend, and on every
 * backend attached afterwards.
 *
 * `stores/watchedThreads.ts` is the door; this is the fan-out. Unlike the
 * lease, the set is SPLIT rather than repeated: a watch frame narrows the
 * entity-filtered channels of ONE connection, so a machine only needs the
 * ids it can push frames about.
 *
 * **Each backend is sent the ids it owns, plus every id whose owner this
 * client does not know.** Both halves are load-bearing and they fail in
 * opposite directions:
 *
 *  - The owner must never be short an id. Withholding one is a pane that
 *    silently stops receiving, which `stores/watchedThreads.ts` calls the
 *    outcome this whole mechanism exists to prevent — and unlike an
 *    over-broad set, nothing later corrects it.
 *  - An unknown id goes to everyone because ./entityIndex.ts is populated
 *    by what this session has listed and been pushed, so a thread opened
 *    from a deep link or painted from the replica has no machine yet
 *    (that module's own doc says so). Home-only would be the routing
 *    fallback's answer, and it is the wrong one here: the id may well
 *    belong to an attached machine that is about to be asked for its
 *    history. The cost is wire bytes on a backend that holds no such
 *    thread, which the next recompute drops once a list or an event names
 *    the owner.
 *
 * A single-backend client is unchanged by construction: nothing is
 * attached beyond home, and home is the only recipient either way.
 *
 * The per-socket bound is each handle's own — `setWatchedThreads` refuses
 * a set past `MAX_WATCH_THREADS` and keeps the previous one — so a split
 * can only ever bring a connection further under it, never over.
 */
export function setWatchedThreadsEverywhere(threadIds: readonly string[]): void {
  watchedThreadIds = [...threadIds];
  for (const entry of entries) sendWatchedThreads(entry);
}

function sendWatchedThreads(entry: Entry): void {
  entry.handle.setWatchedThreads(watchedThreadsFor(entry.id));
}

/**
 * The share of the watched set one backend is sent. Every caller goes
 * through `setWatchedThreadsEverywhere`, and the tests pin the split rule
 * above through it, on each fake client's `setWatchedThreads`.
 */
function watchedThreadsFor(backendId: BackendKey): string[] {
  // The common case is one backend, where the split is the whole set and
  // walking it to prove that is pure cost.
  if (entries.length <= 1) return [...watchedThreadIds];
  const mine: string[] = [];
  for (const id of watchedThreadIds) {
    const owner = threadBackend(id);
    if (owner === undefined || owner === backendId) mine.push(id);
  }
  return mine;
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
function mergeBackendResults(shares: readonly unknown[], homeShare: unknown): unknown {
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
 * ONE attached computer — every desktop with nothing attached, and a phone
 * with one machine — is dispatched directly. The fan-out with one member
 * is that member's call and the merge of one share is the share, so the
 * shortcut changes nothing but the allocations on the app's busiest RPC
 * path. It is the same rule for whichever computer that is: home holds no
 * privilege here.
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
  if (computers.length === 1) return callOneBackend(computers[0], methodId, args, observe);
  const targets = computers.slice();
  // No computer attached is an EMPTY answer, not a failed one: a frontend
  // that has let go of its last computer still lists nothing rather than
  // toasting that nothing answered. An explicit target is refused elsewhere.
  if (targets.length === 0) return mergeBackendResults([], undefined);
  const verify = targets.map((entry) => captureThreadMetadataRead(methodId, entry.id));
  try {
    const settled = await Promise.allSettled(
      targets.map(async (entry) => entry.client.callByID(methodId, args)),
    );
    const shares: unknown[] = [];
    let homeShare: unknown;
    let homeError: unknown = null;
    let firstError: unknown = null;
    let anyFulfilled = false;
    for (let i = 0; i < targets.length; i += 1) {
      const entry = targets[i];
      if (byId.get(entry.id) !== entry) continue;
      let outcome = settled[i];
      try { if (outcome.status === 'fulfilled') verify[i]?.verify(outcome.value); } catch (reason) { outcome = { status: 'rejected', reason }; }
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
  } finally { for (const read of verify) read?.release(); }
}

// The fan-out with one member, without the settled-array walk.
async function callOneBackend(
  entry: Entry,
  methodId: number,
  args: unknown[],
  observe?: (result: unknown, backendId: string) => void,
): Promise<unknown> {
  const verify = captureThreadMetadataRead(methodId, entry.id);
  try {
    const result = await entry.client.callByID(methodId, args);
    // Removed while its reply was pending: the share is dropped exactly as
    // the fan-out drops it, and nothing may index rows to a machine this
    // client is no longer attached to.
    if (byId.get(entry.id) !== entry) throw removedDuringCall();
    verify?.verify(result);
    entry.lastFanoutError = null;
    observe?.(result, entry.id);
    return result;
  } catch (reason) {
    entry.lastFanoutError = reason;
    throw reason;
  } finally { verify?.release(); }
}

/** The rejection every dispatch path issues for a reply from a computer
 *  detached while that reply was pending. */
export function removedDuringCall(): Error {
  return new Error('The computer was removed while waiting for its reply. Check its state before retrying.');
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

// The source is `manifestBackends.defaultBackendDescriptors`: the page's
// own backend plus what the bootstrap manifest published on a desktop, the
// stored endpoint map on a shell. It lives in that leaf so `bootstrap →
// backends` never closes a ring around the `wsClient` singleton, and it is
// asked afresh on every sync. Re-swept whenever the manifest's list moves,
// which is when somebody adds or removes a machine.
onManifestBackendsChanged(syncAttachedBackends);

/**
 * Attach everything the source names, and detach anything held that it no
 * longer does. Idempotent; safe to call again whenever the answer may have
 * moved: the native bridge reporting, a pairing landing, a shell's home
 * learning its identity.
 */
export function syncAttachedBackends(): void {
  let wanted: readonly BackendDescriptor[];
  try {
    wanted = defaultBackendDescriptors();
  } catch (err) {
    // A source that cannot answer names nothing to attach AND nothing to
    // detach. Sweeping on it would forget every computer this client is
    // attached to over a storage hiccup (`homeEndpoint.readEndpointMap`
    // says why the two are different facts), so the registry keeps what it
    // holds until the source can answer again.
    console.warn('transport: keeping the attached computers; their saved addresses could not be read', err);
    return;
  }
  const keep = new Set<string>();
  for (const descriptor of wanted) {
    if (typeof descriptor?.id !== 'string') continue;
    keep.add(descriptor.id);
    attachBackend(descriptor);
  }
  for (const entry of entries.slice()) {
    if (!keep.has(entry.id)) detachBackend(entry.id);
  }
}

/**
 * Test seam: attach `descriptor` over a caller-supplied client, or re-point
 * an id already held at that client, so a suite can stage connections
 * without a socket or a manifest fetch. The re-point swaps the client in
 * place and rewires nothing: a later subscription or call reads the entry's
 * client at that moment, which is all a fake needs.
 *
 * The page's own backend is staged the same way, under
 * `manifestBackends.HOME_DESCRIPTOR`: `src/test/setup.ts` loads the real
 * `wsClient` before any test file's `vi.mock` registers, so mocking that
 * module alone never reaches the registry. What is staged for home is KEPT
 * across `__resetBackendsForTest` — the reset re-attaches home over it —
 * because a file stages it once at module level and expects it for every
 * test; every other staged client is forgotten by the reset.
 */
export function __attachBackendForTest(
  descriptor: BackendDescriptor,
  client: WSClient,
): BackendEntry {
  stagedClients.set(descriptor.id, client);
  const held = byId.get(descriptor.id);
  if (held !== undefined) held.client = client;
  // The ordinary attach from here: a held id takes the staged descriptor
  // the way it takes a re-published one (name, nickname, UUID alias).
  return attachBackend(descriptor);
}

/**
 * Test seam: back to what the default source names — home alone, on a
 * desktop suite — with every "everywhere" fact cleared and the manifest's
 * published list forgotten, so nothing a test published is re-attached over
 * a real connection by the sync below.
 */
export function __resetBackendsForTest(): void {
  for (const entry of entries.slice()) {
    if (!entry.home) detachBackend(entry.id);
  }
  for (const id of [...stagedClients.keys()]) {
    if (id !== HOME_BACKEND) stagedClients.delete(id);
  }
  installedProver = null;
  clientLease = 'active';
  watchedThreadIds = [];
  screenPresence = null;
  __resetManifestBackendsForTest();
  syncAttachedBackends();
  const home = byId.get(HOME_BACKEND);
  if (home !== undefined) home.lastFanoutError = null;
}

// The page's own backend attaches HERE, at module evaluation, and that is
// deliberate: stores subscribe through `Events.On` while THEY evaluate, a
// standing subscription is what opens a socket, and the boot sequence is
// what decides when that happens — so home has to be attached before the
// first store runs, as it always was. The source answers for this client
// class (a shell without a stored legacy slot attaches nothing here), and
// every later sync reconciles from the same answer.
syncAttachedBackends();
