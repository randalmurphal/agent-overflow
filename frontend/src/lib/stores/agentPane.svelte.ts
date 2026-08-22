// Scope state for the `agent` companion pane — the thread view filtered to
// one subagent launch row (docs/specs/agent-visibility.md Q4/Q4b/Q5).
//
// There is exactly ONE agent pane per source thread pane: opening another
// node swaps the scope in place rather than stacking a second pane. The
// state is therefore keyed by SOURCE pane id, mirroring
// reviewPane.svelte.ts's `statesBySourcePane` — including its thread-mismatch
// rule, where a state built for a departed thread is replaced rather than
// reused, so correctness never depends on Svelte destroying the old panel
// body before creating the new one.
//
// The breadcrumb's first entry is always the thread itself (`itemId: ''`,
// label `main`); descending into a child pushes an entry, and clicking one
// pops back to it. Popping all the way to the root leaves the EMPTY scope —
// the degenerate "you are back in the main thread" state, which the pane
// body answers by closing itself. Persistence never restores an empty
// scope (paneLayoutPersistence.ts drops such a pane), so the empty scope is
// a transient, never a thing a reload can land in.

import { addPaneDestroyedObserver } from './panes.svelte';
import { isCompanionOpen, openCompanion } from './companionPanes.svelte';
import { requestPaneLayoutPersistence } from './paneLayout.svelte';
import type { AgentPaneBreadcrumbEntry, AgentPaneScopeSnapshot } from '../types/settings';

/** Label the root breadcrumb entry carries — the thread itself. */
export const AGENT_SCOPE_ROOT_LABEL = 'main';

function rootEntry(): AgentPaneBreadcrumbEntry {
  return { itemId: '', label: AGENT_SCOPE_ROOT_LABEL };
}

export interface AgentPaneState {
  /** Thread this scope belongs to. Fixed for the state's lifetime. */
  readonly threadId: string;
  /**
   * Launch item id the pane's thread view is filtered to, or `''` for the
   * root (no node scoped). Always equals the last breadcrumb entry's
   * `itemId`.
   */
  readonly scopeItemId: string;
  /** `main › code-review › Angle B`, root first. Never empty. */
  readonly breadcrumb: readonly AgentPaneBreadcrumbEntry[];
  /**
   * Scope to `itemId` from OUTSIDE the pane (a card in the source
   * timeline): the trail restarts at the root, so reopening on an
   * unrelated node cannot inherit a stale ancestry.
   */
  setScope(itemId: string, label: string): void;
  /**
   * Scope to `itemId` from INSIDE the pane (a child card in the current
   * node's transcript): the trail grows by one hop. Re-pushing the node
   * already scoped is a no-op, so a double click cannot duplicate a hop.
   */
  pushScope(itemId: string, label: string): void;
  /** Breadcrumb click: truncate the trail to `index` and scope to it. */
  popTo(index: number): void;
  /** Back to the root: empty scope, trail of one. */
  reset(): void;
}

const statesBySourcePane = new Map<string, AgentPaneState>();
let unsubscribePaneDestroyed: (() => void) | null = null;

// Registered on first use rather than from an app-level install hook: the
// only thing this observer does is drop a Map entry, and lazy registration
// keeps the agent store out of the eager startup graph (its pane body is a
// lazily-imported chunk). Idempotent — `installCompanionPanes`'s cascade
// close and this run independently, in either order.
function ensurePaneDestroyedObserver(): void {
  if (unsubscribePaneDestroyed) return;
  unsubscribePaneDestroyed = addPaneDestroyedObserver((destroyedPaneId) => {
    statesBySourcePane.delete(destroyedPaneId);
  });
}

// True only while layout RESTORE is seeding a state. A save requested then
// would snapshot the half-built layout (thread panes restored, companions
// not yet), writing a layout with no companions in it at all.
let seeding = false;

// The scope lives here, not on the layout item, but the pane-layout SNAPSHOT
// is what persists it — so every scope change has to ask for a save the way
// a layout mutation does.
function noteScopeChanged(): void {
  if (seeding) return;
  requestPaneLayoutPersistence();
}

function createAgentPaneState(threadId: string): AgentPaneState {
  let scopeItemId = $state('');
  let breadcrumb: AgentPaneBreadcrumbEntry[] = $state([rootEntry()]);

  // Declared as functions rather than object-literal methods so the
  // cross-calls below (pushScope → popTo, setScope → reset) never depend on
  // `this` — a destructured `const { pushScope } = state` stays correct.
  function reset(): void {
    if (scopeItemId === '' && breadcrumb.length === 1) return;
    scopeItemId = '';
    breadcrumb = [rootEntry()];
    noteScopeChanged();
  }

  function setScope(itemId: string, label: string): void {
    if (!itemId) {
      reset();
      return;
    }
    scopeItemId = itemId;
    breadcrumb = [rootEntry(), { itemId, label }];
    noteScopeChanged();
  }

  function popTo(index: number): void {
    if (index < 0 || index >= breadcrumb.length) return;
    if (index === breadcrumb.length - 1) return;
    breadcrumb = breadcrumb.slice(0, index + 1);
    scopeItemId = breadcrumb[index].itemId;
    noteScopeChanged();
  }

  function pushScope(itemId: string, label: string): void {
    if (!itemId) return;
    if (itemId === scopeItemId) return;
    // Descending into a node already ON the trail is a pop, not a second
    // hop — otherwise `main › a › b › a` becomes representable and the
    // breadcrumb stops describing an ancestry.
    const existing = breadcrumb.findIndex((entry) => entry.itemId === itemId);
    if (existing >= 0) {
      popTo(existing);
      return;
    }
    scopeItemId = itemId;
    breadcrumb = [...breadcrumb, { itemId, label }];
    noteScopeChanged();
  }

  return {
    get threadId() {
      return threadId;
    },
    get scopeItemId() {
      return scopeItemId;
    },
    get breadcrumb() {
      return breadcrumb;
    },
    setScope,
    pushScope,
    popTo,
    reset,
  };
}

/**
 * The agent scope state for a source pane, created on first read.
 *
 * Side-effectful — it writes the registry, and a thread mismatch REPLACES
 * the entry — so callers must invoke it from an init or event context,
 * never inside a `$derived` (that would be a `state_unsafe_mutation` crash
 * mid-render). See ReviewPane.svelte's `state_referenced_locally` note for
 * the same constraint on the review precedent.
 */
export function agentStateForPane(sourcePaneId: string, threadId: string): AgentPaneState {
  ensurePaneDestroyedObserver();
  const existing = statesBySourcePane.get(sourcePaneId);
  if (existing && existing.threadId === threadId) return existing;
  const state = createAgentPaneState(threadId);
  statesBySourcePane.set(sourcePaneId, state);
  return state;
}

/**
 * Non-creating read for persistence. Answers null when the pane has no
 * scope state, when the state belongs to another thread, or when the scope
 * is empty — none of those describe a pane worth restoring.
 */
export function agentScopeForPane(
  sourcePaneId: string,
  threadId: string,
): AgentPaneScopeSnapshot | null {
  const state = statesBySourcePane.get(sourcePaneId);
  if (!state || state.threadId !== threadId) return null;
  if (!state.scopeItemId) return null;
  return {
    scopeItemId: state.scopeItemId,
    breadcrumb: state.breadcrumb.map((entry) => ({ ...entry })),
  };
}

/**
 * True when the OPEN agent pane for `sourcePaneId` is scoped to
 * `itemId` — or holds it on the breadcrumb trail. The thread pane's
 * subagent memory consults this alongside card expansion: rows under a
 * scope the reader is looking at (or one hop up the trail from) must not
 * fold out of pane memory the way rows under a collapsed card do.
 * Requires the companion to actually be open, because scope state can
 * outlive a generic companion close.
 */
export function agentPaneScopeTrailHolds(
  sourcePaneId: string,
  threadId: string,
  itemId: string,
): boolean {
  if (!itemId) return false;
  const state = statesBySourcePane.get(sourcePaneId);
  if (!state || state.threadId !== threadId || !state.scopeItemId) return false;
  if (!isCompanionOpen(sourcePaneId, 'agent')) return false;
  return state.breadcrumb.some((entry) => entry.itemId === itemId);
}

/**
 * The OUTERMOST scoped node on the open agent pane's breadcrumb trail
 * (`''` when no pane is open for this pane+thread). Descending inside
 * the pane only ever enters the current scope's subtree, so the trail is
 * an ancestry chain and the first non-root entry's subtree covers every
 * scope on it. The thread pane's row-UI prune widens its retention with
 * that subtree — state under rows the agent pane (or a pop back up its
 * trail) is showing must not be disposed by the chat timeline's
 * bounded-memory pass.
 */
export function agentPaneRetainedRootScope(
  sourcePaneId: string,
  threadId: string,
): string {
  const state = statesBySourcePane.get(sourcePaneId);
  if (!state || state.threadId !== threadId || !state.scopeItemId) return '';
  if (!isCompanionOpen(sourcePaneId, 'agent')) return '';
  return state.breadcrumb.find((entry) => entry.itemId !== '')?.itemId ?? '';
}

/**
 * Restore a persisted scope onto a source pane. Used by layout restore
 * only; the snapshot has already been validated (non-empty scope, trail
 * ending at it) by the persistence parser.
 */
export function seedAgentStateForPane(
  sourcePaneId: string,
  threadId: string,
  snapshot: AgentPaneScopeSnapshot,
): AgentPaneState | null {
  if (!snapshot.scopeItemId) return null;
  seeding = true;
  try {
    const state = agentStateForPane(sourcePaneId, threadId);
    state.reset();
    for (const entry of snapshot.breadcrumb) {
      if (!entry.itemId) continue;
      state.pushScope(entry.itemId, entry.label);
    }
    if (state.scopeItemId !== snapshot.scopeItemId) {
      state.setScope(snapshot.scopeItemId, snapshot.breadcrumb.at(-1)?.label ?? '');
    }
    return state;
  } finally {
    seeding = false;
  }
}

export function disposeAgentStateForPane(sourcePaneId: string, expectedThreadId?: string): void {
  const current = statesBySourcePane.get(sourcePaneId);
  if (!current) return;
  if (expectedThreadId && current.threadId !== expectedThreadId) return;
  statesBySourcePane.delete(sourcePaneId);
}

/**
 * Open (or re-scope) the agent companion for `sourcePaneId` on the node
 * `launchItemId` launched. One pane per source pane: a second open swaps
 * the scope of the pane already there — spec Q4b, "no stacking".
 */
export function openAgentCompanion(
  sourcePaneId: string,
  threadId: string,
  launchItemId: string,
  label: string,
): AgentPaneState | null {
  if (!threadId || !launchItemId) return null;
  const companion = openCompanion(sourcePaneId, 'agent');
  if (!companion) return null;
  const state = agentStateForPane(sourcePaneId, threadId);
  state.setScope(launchItemId, label);
  return state;
}

export function __resetAgentPaneStateForTest(): void {
  seeding = false;
  statesBySourcePane.clear();
  unsubscribePaneDestroyed?.();
  unsubscribePaneDestroyed = null;
}
