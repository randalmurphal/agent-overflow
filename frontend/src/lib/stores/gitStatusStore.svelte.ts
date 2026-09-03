// Live git status, keyed by workspace.
//
// Git status is a fact about a CHECKOUT, so the workspace path is the entity
// and every consumer derives from one entry: the chat header's diff/PR
// badges, the commit/push control, the Ship Changes wizard, the branch
// picker's dirty bit, and the review pane's PR reference. Two panes on one
// worktree (project-root threads default to it; "implement this plan in a
// new thread" inherits the source worktree) therefore cannot disagree about
// whether there is anything to commit — which they could, for minutes, when
// each pane held a private copy filtered by its own subscription id.
//
// The backend pumps one `git:status` stream per canonical cwd and addresses
// its events by that cwd. This module maps canonical cwd back to the local
// key(s) that asked for it, so the wire stays entity-keyed and the store
// stays keyed on what the frontend actually knows: the workspace path a
// pane is pointed at — a persisted thread's or a draft placeholder's alike.

import type { GitStatus, WorkspaceRef } from '../types/git';
import type { Thread } from '../types/models';
import {
  GetGitStatus,
  GitStatusSubscribe,
  GitStatusUnsubscribe,
  UpdateThreadBranch,
  type GitStatusSubscriptionResult,
} from './bindings';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import { isTransportClassError } from './transportStatus.svelte';
import { hasScope } from '../transport/scopes';
import { wailsEventOn } from './wailsEvents';
import { errString } from '../utils/errors';
import {
  composeWorkspaceKey,
  workspaceKeyBackend,
  workspaceKeyForThread,
  workspaceKeyPath,
  workspaceRefForThread,
} from '../utils/workspaceKey';
import { backendKeyForOrigin, withBackendTarget } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';

/** What a source needs from whoever is holding the key. */
export interface GitStatusCtx {
  /**
   * The checkout this key names, as the subscribe RPC takes it. May be
   * declared as a getter, and is READ AT SOURCE TIME: one attacher holds one
   * workspace key across every thread (and placeholder) it shows, so a
   * re-source (reconnect, retry) has to run against what the pane points at
   * now, not what it happened to point at when it attached.
   *
   * Null is a pane that stopped naming a checkout under a re-source; the
   * source fails loudly rather than subscribing to nothing.
   */
  readonly workspace: WorkspaceRef | null;
}

// Wire payload shape for "git:status" events. Wails generates no TS type for
// event payloads, so the shape is declared locally and kept in sync with
// GitStatusEvent in app_gitwatch.go.
interface GitStatusEvent {
  cwd: string;
  status: GitStatus;
}

// `${backendId} ${canonical cwd}` → the local keys subscribed through it,
// each stamped with the source RUN that installed it. One-to-many because
// two spellings of one directory (a symlinked path and its resolution)
// canonicalize to the same cwd on the backend while staying distinct thread
// rows here.
//
// Keyed by BACKEND as well as cwd, and for the reason
// `utils/workspaceKey.ts` states: canonicalization happens on the backend,
// so two machines holding the same checkout report the same cwd. Routing a
// `git:status` frame by cwd alone would paint one machine's status onto the
// other's header — silently, and only ever on a client attached to both.
//
// The owner stamp is what makes the map safe under re-sourcing. A superseded
// run (invalidate, reconnect, retry) resolves LATE and then runs its own
// cleanup; without ownership that cleanup deletes the alias the live run
// installed, and the key goes on holding a subscription whose `git:status`
// pushes route nowhere — a header frozen at whatever it last saw, healed
// only by a full re-attach.
type AliasOwner = symbol;
const localKeysByCwd = new Map<string, Map<string, AliasOwner>>();

function addAlias(cwdKey: string, key: string, owner: AliasOwner): void {
  let keys = localKeysByCwd.get(cwdKey);
  if (!keys) {
    keys = new Map<string, AliasOwner>();
    localKeysByCwd.set(cwdKey, keys);
  }
  keys.set(key, owner);
}

function removeAlias(cwdKey: string, key: string, owner: AliasOwner): void {
  const keys = localKeysByCwd.get(cwdKey);
  if (keys?.get(key) !== owner) return;
  keys.delete(key);
  if (keys.size === 0) localKeysByCwd.delete(cwdKey);
}

const store = createEntityStore<GitStatus, GitStatusCtx>({
  name: 'gitStatus',
  source: async ({ key, getCtx, apply, signal }) => {
    const owner: AliasOwner = Symbol(key);
    const backend = workspaceKeyBackend(key);
    const workspace = getCtx().workspace;
    if (workspace === null) {
      throw new Error(`git status: no workspace to subscribe for ${key}`);
    }
    // Pinned to the KEY's backend rather than routed from the ref: the key
    // is the entity, and the subscription id the answer carries belongs to
    // whichever connection minted it, so the two must be one machine.
    const result = (await withBackendTarget(backend, () =>
      GitStatusSubscribe(workspace),
    )) as GitStatusSubscriptionResult;
    const cwdKey = composeWorkspaceKey(backend, result.cwd);
    // Only a run that is still the live one may claim the alias; a
    // superseded run's cleanup then finds an owner that is not its own and
    // leaves the live routing alone.
    if (!signal.aborted) addAlias(cwdKey, key, owner);
    apply(result.status as GitStatus);
    return async () => {
      removeAlias(cwdKey, key, owner);
      try {
        // A subscription id is meaningful only on the connection that
        // minted it, and nothing in the id says which that was — hence the
        // pin rather than a route.
        await withBackendTarget(backend, () => GitStatusUnsubscribe(result.id));
      } catch (err) {
        // A dead wire needs no unsubscribe: the backend releases every
        // subscription a connection held when it drops. Anything else is a
        // real failure to release and must be seen.
        if (!isTransportClassError(err)) throw err;
      }
    };
  },
  onApply: (key, value, prev) => {
    if (!value.isRepo) return;
    const branch = value.branch ?? '';
    // First observation for a key always writes: the thread rows' cached
    // branch may predate anything this session saw. That costs one UPDATE
    // per attach and nothing else — the backend matches only rows whose
    // branch actually differs, so the usual "already correct" case reads
    // nothing back and returns no rows for this side to sync.
    if (prev !== null && (prev.branch ?? '') === branch) return;
    queueBranchPersist(key, branch);
  },
});

// ONE listener for the whole app. Events are addressed by canonical cwd;
// every local key aliased to it observes the same status.
let gitStatusEventOff: (() => void) | null = null;

function installGitStatusEventListener(): void {
  gitStatusEventOff?.();
  gitStatusEventOff = wailsEventOn<GitStatusEvent>('git:status', (payload, origin) => {
    if (!payload?.cwd) return;
    // The frame names a directory; the connection it arrived on names the
    // machine. Neither is enough alone.
    const keys = localKeysByCwd.get(
      composeWorkspaceKey(backendKeyForOrigin(origin.backendId), payload.cwd),
    );
    if (!keys) return;
    for (const key of keys.keys()) store.apply(key, payload.status);
  });
}

installGitStatusEventListener();

// ---------------------------------------------------------------------------
// Pane bridge
// ---------------------------------------------------------------------------

// Reconciliation has to reach the panes, and this module must not import
// them: `panes → thread → gitStatusStore` already exists, so importing panes
// from here closes a cycle whose init order decides whether an event handler
// registers. The pane side arms the bridge when IT loads (same shape as
// `setWorkflowsOverlayExclusion`), so there is no registration order to get
// right and no test reset that can disarm it.
export interface GitStatusPaneBridge {
  /** Push a persisted thread row into the registry and every pane on it. */
  syncThread(thread: Thread): void;
  /** Surface a failure on the panes still looking at that workspace. Takes
   *  the WORKSPACE KEY (`utils/workspaceKey.ts`), not a bare path. */
  reportWorkspaceError(workspaceKey: string, message: string): void;
}

let paneBridge: GitStatusPaneBridge | null = null;

export function setGitStatusPaneBridge(bridge: GitStatusPaneBridge): void {
  paneBridge = bridge;
}

function bridge(): GitStatusPaneBridge | null {
  if (paneBridge === null) {
    console.error('gitStatus: no pane bridge installed — import stores/panes.svelte to arm it');
  }
  return paneBridge;
}

// ---------------------------------------------------------------------------
// Observed-branch reconciliation
// ---------------------------------------------------------------------------

// Queued per workspace and collapsed to the latest value: a burst of branch
// flips must not become a burst of writes, and an older observation must
// never land after a newer one. Queued rather than awaited inline so
// persistence never blocks status application.
//
// The write itself is keyed on the workspace (UpdateThreadBranch), which is
// what makes it safe to issue without a thread lock: a thread that has since
// switched worktrees is no longer in this workspace and simply is not
// matched. Nothing is written optimistically, so there is no stale local
// value to repair afterwards — the returned rows are the truth.
const queuedBranchByWorkspace = new Map<string, string>();
let branchPersistRunning = false;

function queueBranchPersist(workspaceKey: string, branch: string): void {
  queuedBranchByWorkspace.set(workspaceKey, branch);
  if (branchPersistRunning) return;
  branchPersistRunning = true;
  void drainBranchPersistQueue();
}

async function drainBranchPersistQueue(): Promise<void> {
  try {
    while (queuedBranchByWorkspace.size > 0) {
      const next = queuedBranchByWorkspace.entries().next();
      if (next.done) return;
      const [workspaceKey, branch] = next.value;
      queuedBranchByWorkspace.delete(workspaceKey);
      try {
        // No rows back is the common answer, not an edge case: the backend
        // writes only rows whose branch actually moved, so a first
        // observation that agrees with the cache costs zero syncs and zero
        // reactive churn here.
        // Path-keyed, so the backend is pinned from the key. Un-pinned this
        // would write one machine's observed branch onto the rows of every
        // thread the OTHER machine holds in the identically-named
        // directory.
        const rows = (await withBackendTarget(workspaceKeyBackend(workspaceKey), () =>
          UpdateThreadBranch(workspaceKeyPath(workspaceKey), branch),
        )) as Thread[] | null;
        if (rows && rows.length > 0) {
          const paneBridge = bridge();
          for (const row of rows) paneBridge?.syncThread(row);
        }
      } catch (err) {
        console.error('Failed to persist observed git branch:', err);
        reportBranchPersistFailure(workspaceKey, err);
      }
    }
  } finally {
    branchPersistRunning = false;
  }
}

// Surfaced on the panes still looking at that workspace. A pane that has
// moved on is not shown an error about a checkout it left.
function reportBranchPersistFailure(workspaceKey: string, err: unknown): void {
  bridge()?.reportWorkspaceError(workspaceKey, `Failed to update thread branch: ${errString(err)}`);
}

// ---------------------------------------------------------------------------
// Public surface
// ---------------------------------------------------------------------------

// The answer for a session that was not granted `git:operate`: no value,
// no error, nothing to release. Every RPC this store owns carries that
// scope (internal/transport/methods_gen.go), so sourcing would be one
// refusal per workspace per mount — and the store would then hold the
// refusal as a persistent `statusError`, which the workspace strip
// renders. A predicted absence is not a failure to report; the strip's
// no-status state is the honest rendering of "this session does not see
// git".
const NO_GIT_STATUS: EntityAttachment<GitStatus> = {
  get current() {
    return null;
  },
  get error() {
    return null;
  },
  release() {},
};

/** Refcounted attach for a workspace. Release when the consumer unmounts. */
export function attachGitStatus(key: string, ctx: GitStatusCtx): EntityAttachment<GitStatus> {
  if (!hasScope('git:operate', workspaceKeyBackend(key))) return NO_GIT_STATUS;
  assertAttachableWhileSeeding(key);
  return store.attach(key, ctx);
}

/** Read a workspace's status without attaching. Reactive. */
export function peekGitStatus(key: string | null): GitStatus | null {
  return key === null ? null : store.peek(key);
}

/** Read a workspace's error without attaching. Reactive; null when healthy. */
export function peekGitStatusError(key: string | null): string | null {
  return key === null ? null : store.peekError(key);
}

/**
 * One-shot refresh after a git action, so the UI catches up without waiting
 * on the ~250ms fs-watcher debounce. Routed through the store's apply
 * chokepoint, so branch reconciliation happens here too; the backend also
 * pushes the same refresh to every other client on this workspace.
 *
 * `currentKey` is re-read after the await and must still name `key`: the
 * caller can be re-pointed at another checkout mid-flight, and applying the
 * answer anyway would both paint the wrong status and persist the wrong
 * branch onto every thread still in the old workspace. The CALLER supplies
 * it, because the caller is what holds the pane this refresh is about;
 * resolving it through the pane registry made the verdict depend on UI mount
 * state and dragged this module into the pane graph.
 */
export async function refreshGitStatus(
  key: string,
  workspace: WorkspaceRef,
  currentKey: () => string | null,
): Promise<void> {
  if (!hasScope('git:operate', workspaceKeyBackend(key))) return;
  try {
    const result = (await GetGitStatus(workspace)) as GitStatus;
    if (currentKey() !== key) return;
    store.apply(key, result);
  } catch (err) {
    if (currentKey() !== key) return;
    store.applyError(key, err);
  }
}

/**
 * Per-consumer read view over the shared store, resolving its key on every
 * read so a thread switch or worktree move re-points it with no bookkeeping.
 * Holds no subscription — attaching is the mounting consumer's job.
 */
export interface GitStatusView {
  /** Latest observed status for the current workspace, or null. */
  readonly status: GitStatus | null;
  /** Failure message for the current workspace, or null when healthy. */
  readonly statusError: string | null;
  /** One-shot refresh used after git actions. */
  refreshNow(): Promise<void>;
}

export function createGitStatusView(getThread: () => Thread | null): GitStatusView {
  const key = (): string | null => workspaceKeyForThread(getThread());
  return {
    get status() {
      return peekGitStatus(key());
    },
    get statusError() {
      return peekGitStatusError(key());
    },
    async refreshNow() {
      const workspacePath = key();
      const workspace = workspaceRefForThread(getThread());
      if (workspacePath === null || workspace === null) return;
      await refreshGitStatus(workspacePath, workspace, key);
    },
  };
}

/** Diagnostics / tests: the workspaces currently held. */
export function gitStatusKeys(): string[] {
  return store.keys();
}

/**
 * Transport-gap recovery: re-source every held workspace.
 *
 * `git:status` is edge-triggered — one frame per change to a checkout — so a
 * dropped frame leaves every consumer of that workspace stale until the user
 * happens to touch the repo again. The gap signal carries no cwd, so recovery
 * is blanket; that is the correct coarse answer rather than a shortcut,
 * because live keys are bounded by what is mounted (a handful of workspaces)
 * and re-sourcing KEEPS each key's last status, so no badge blinks on the way
 * to the fresh one.
 */
export function resyncGitStatusAfterGap(): void {
  store.invalidateAll();
}

// ---------------------------------------------------------------------------
// Test seams
// ---------------------------------------------------------------------------

// Component tests whose subject READS git status but never attaches (the
// attacher is ChatHeaderActions) need to put a workspace into an observed
// state with no backend behind it. Seeding suspends the store first: such a
// test has no GitStatusSubscribe to answer, and the failing source would
// paint an error over the seed. The reference is held so the entry exists at
// all; __resetGitStatusStoreForTest releases it and lifts the suspension.
const testHolds = new Map<string, EntityAttachment<GitStatus>>();
let seedingForTest = false;

/**
 * A seed may be written with a bare PATH, which reads as "the workspace at
 * this path on the only backend this test has". Every seeding test holds
 * one connection — a component test has no registry to attach a second to —
 * and spelling the home prefix at each call site would be ceremony that
 * says nothing. Anything already carrying a backend is left alone, so a
 * test that DOES stage two backends still seeds them apart.
 */
function seedKey(key: string): string {
  return key.includes(' ') ? key : composeWorkspaceKey(HOME_BACKEND, key);
}

function ensureTestHold(key: string): void {
  if (!seedingForTest) {
    store.suspend();
    seedingForTest = true;
  }
  if (!testHolds.has(key)) {
    // The store is suspended while seeding, so this reference never sources
    // and the ref is never read.
    testHolds.set(key, store.attach(key, { workspace: null }));
  }
}

/**
 * The seam's suspension is store-wide, so a genuine attach in the same test
 * file would register a reference that silently never sources — a component
 * rendering a blank badge forever, with nothing to point at. Refuse it
 * instead, naming the seam: either seed that workspace too, or the test
 * wants a real subscribe mock and no seeding at all.
 */
function assertAttachableWhileSeeding(key: string): void {
  if (!seedingForTest || testHolds.has(key)) return;
  throw new Error(
    `gitStatus: __seedGitStatusForTest has suspended the store, so attaching ${key} `
      + 'would never source. Seed that workspace too, or drop the seam and mock GitStatusSubscribe.',
  );
}

export function __seedGitStatusForTest(key: string, status: GitStatus): void {
  const held = seedKey(key);
  ensureTestHold(held);
  store.apply(held, status);
}

export function __seedGitStatusErrorForTest(key: string, message: string): void {
  const held = seedKey(key);
  ensureTestHold(held);
  store.applyError(held, new Error(message));
}

/**
 * Test seam: drop every entry, alias, and queued write, as a fresh module
 * load would, and re-arm the `git:status` listener (the shared wails-runtime
 * mock clears every subscriber between tests, and this module registers at
 * load time — which happens once per file).
 *
 * suspend() releases every unheld entry; resetAll() then lifts the
 * suspension. An entry that survives both is one a test attached and never
 * released — resetAll re-sources it against the next test's binding mocks,
 * which is exactly the noise that should make the leak findable.
 */
export function __resetGitStatusStoreForTest(): void {
  for (const hold of testHolds.values()) hold.release();
  testHolds.clear();
  seedingForTest = false;
  store.suspend();
  store.resetAll();
  localKeysByCwd.clear();
  queuedBranchByWorkspace.clear();
  branchPersistRunning = false;
  installGitStatusEventListener();
}
