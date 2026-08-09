import type { Thread } from '../types/models';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import {
  addPaneLayoutItem,
  averagePaneWidthPx,
  getPaneLayoutItems,
  movePaneLayoutItem,
  removePaneLayoutItem,
  sourcePaneIdOf,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import { getThreadById, replaceThread as replaceThreadInRegistry } from './threads.svelte';
import { setGitStatusPaneBridge } from './gitStatusStore.svelte';
import { workspaceKeyForThread } from '../utils/workspaceKey';
import { REVEAL_PANE_EVENT } from './eventNames';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';

// Active panes, keyed by pane ID. PaneHost mounts panes from layout order;
// command routing and sidebar actions resolve explicit pane targets through
// this registry.
let panes: Map<string, ThreadPane> = $state(new Map());
let focusedPaneId: string | null = $state('main');
export type PaneActivation = 'preview' | 'committed';
let paneActivationById: Map<string, PaneActivation> = $state(new Map());
let nextGeneratedPaneId = 1;
let panePersistenceHandler: (() => void) | null = null;
// Notified with a paneId after that pane (a ThreadPane) is destroyed.
// Companion stores register here to cascade-close panes paired to a closing
// source pane. Kept as a registration hook (not direct imports) so
// panes.svelte.ts never depends on companion stores — the dependency runs one
// way only (companion stores read the pane registry, never the reverse).
let paneDestroyedObservers: Array<(paneId: string) => void> = [];

export function setPanePersistenceHandler(handler: (() => void) | null): void {
  panePersistenceHandler = handler;
}

export function addPaneDestroyedObserver(observer: (paneId: string) => void): () => void {
  paneDestroyedObservers = [...paneDestroyedObservers, observer];
  return () => {
    paneDestroyedObservers = paneDestroyedObservers.filter((existing) => existing !== observer);
  };
}

function requestPanePersistence(): void {
  panePersistenceHandler?.();
}

/**
 * Ask PaneHost to horizontally scroll the pane into view. Reveal is
 * deliberately decoupled from focus: `focusPane` never scrolls, and only
 * explicit-intent sites (keyboard pane nav, sidebar/palette thread opens,
 * drag-drop, pane reorder, companion open, click on an unfocused pane)
 * call this. DOM `focusin` must never reach it — window re-activation and
 * focus-trap restores re-fire focus events, and revealing on those yanked
 * the strip away from wherever the user had scrolled.
 *
 * One reveal lives outside this event: PaneHost itself snaps the focused
 * pane into view when the strip (re)appears (startup restore, global
 * settings surface closing) — it cannot route through here because
 * PaneHost mounts after the restore flow runs, so no listener exists yet.
 */
export function revealPane(paneId: string): void {
  if (typeof window === 'undefined' || !paneId) return;
  window.dispatchEvent(new CustomEvent(REVEAL_PANE_EVENT, {
    detail: { paneId },
  }));
}

function hasLayoutPane(paneId: string): boolean {
  return getPaneLayoutItems().some((item) => item.paneId === paneId);
}

function addThreadPaneToLayout(paneId: string, insertIndex?: number): void {
  if (hasLayoutPane(paneId)) return;
  addPaneLayoutItem({
    id: paneId,
    paneId,
    kind: 'thread',
    widthPx: averagePaneWidthPx(),
  }, insertIndex, { persist: false });
}

function resolveNewPaneInsertIndex(insertIndex?: number): number {
  if (insertIndex !== undefined) return insertIndex;
  const layoutItems = getPaneLayoutItems();
  const focusedIndex = focusedPaneId
    ? layoutItems.findIndex((item) => item.paneId === focusedPaneId)
    : -1;
  return focusedIndex >= 0 ? focusedIndex + 1 : layoutItems.length;
}

/**
 * Ensure the pane is mounted in the layout grid. Idempotent: if the
 * pane is already present, no layout mutation happens. Used by the
 * draft-placeholder open flow (`openDraftThreadForProject`) so a
 * pane can host the composer before any real thread row exists.
 */
export function ensurePaneInLayout(paneId: string): void {
  if (hasLayoutPane(paneId)) return;
  addThreadPaneToLayout(paneId);
  focusedPaneId = paneId;
  revealPane(paneId);
  requestPanePersistence();
}

function nextPaneId(): string {
  if (panes.size === 0) return 'main';
  let id = `pane-${nextGeneratedPaneId}`;
  while (panes.has(id) || hasLayoutPane(id)) {
    nextGeneratedPaneId += 1;
    id = `pane-${nextGeneratedPaneId}`;
  }
  nextGeneratedPaneId += 1;
  return id;
}

function orderedPaneIds(): string[] {
  return getPaneLayoutItems()
    .map((item) => item.paneId)
    .filter((paneId) => panes.has(paneId));
}

function registerPane(id: string, pane: ThreadPane, activation: PaneActivation = 'committed'): ThreadPane {
  panes = new Map(panes).set(id, pane);
  paneActivationById = new Map(paneActivationById).set(id, activation);
  return pane;
}

export function ensureMainPane(): ThreadPane {
  let main = panes.get('main');
  if (!main) {
    main = createThreadPane({ paneId: 'main' });
    registerPane('main', main, 'committed');
  }
  return main;
}

export function getMainPane(): ThreadPane {
  const main = panes.get('main');
  if (!main) {
    throw new Error('Pane registry is missing the main pane.');
  }
  return main;
}

export function createPane(id: string): ThreadPane {
  const existing = panes.get(id);
  if (existing) return existing;
  const pane = createThreadPane({ paneId: id });
  return registerPane(id, pane, 'committed');
}

export function getPane(id: string): ThreadPane | undefined {
  return panes.get(id);
}

export function registerPaneForTest(id: string, pane: ThreadPane): void {
  registerPane(id, pane, 'committed');
}

export function getFocusedPane(): ThreadPane {
  const pane = getFocusedPaneOrNull();
  if (!pane) {
    throw new Error(`Focused pane "${focusedPaneId ?? 'none'}" is not registered.`);
  }
  return pane;
}

// Resolve a layout pane id to the thread pane that owns it: thread panes
// resolve to themselves, companion panes (plan/review/design-preview/
// take-control) to their sourcePaneId. Resolution goes through the layout
// store's membership-keyed lookup — never the raw items array — so this
// store never depends on companion stores AND reactive callers (ChatView's
// per-frame read-mark gates) don't re-run on divider-drag width churn.
function resolveThreadPaneId(paneId: string | null): string | null {
  if (!paneId) return null;
  if (panes.has(paneId)) return paneId;
  return sourcePaneIdOf(paneId);
}

/**
 * The ThreadPane that thread-scoped actions (composer targeting, thread
 * opens, command context) should act on. When a companion pane is focused
 * this resolves to its source thread pane — the companion belongs to that
 * thread. Pane-scoped commands (close/move) must use the raw
 * `getFocusedPaneId()` instead so they act on the companion itself.
 */
export function getFocusedPaneOrNull(): ThreadPane | null {
  const threadPaneId = resolveThreadPaneId(focusedPaneId);
  return threadPaneId ? panes.get(threadPaneId) ?? null : null;
}

/** `getFocusedPaneOrNull()`'s pane id, without requiring the pane object. */
export function getFocusedThreadPaneId(): string | null {
  return resolveThreadPaneId(focusedPaneId);
}

/**
 * Set logical focus. Accepts any mounted layout pane — thread panes and
 * companions alike. Never scrolls: callers that represent genuine
 * navigation intent pair this with `revealPane`.
 */
export function focusPane(id: string): void {
  if (focusedPaneId === id) return;
  if (!panes.has(id) && !hasLayoutPane(id)) return;
  focusedPaneId = id;
  requestPanePersistence();
}

export function getFocusedPaneId(): string | null {
  return focusedPaneId;
}

export function getAllPanes(): Map<string, ThreadPane> {
  return panes;
}

export function listPanes(): ThreadPane[] {
  return Array.from(panes.values());
}

export function iterPanes(): IterableIterator<ThreadPane> {
  return panes.values();
}

export function forEachPane(fn: (pane: ThreadPane) => void): void {
  for (const pane of panes.values()) {
    fn(pane);
  }
}

export function panesShowingThread(threadId: string): ThreadPane[] {
  return listPanes().filter((pane) => pane.threadId === threadId);
}

export function forPanesShowingThread(
  threadId: string,
  fn: (pane: ThreadPane) => void,
): void {
  for (const pane of panes.values()) {
    if (pane.threadId !== threadId) continue;
    fn(pane);
  }
}

export function isThreadVisible(threadId: string): boolean {
  return findPaneShowingThread(threadId) !== null;
}

/**
 * Every pane currently hosting a draft placeholder for `projectId`.
 *
 * A placeholder has no thread row, so nothing the backend broadcasts can
 * reach it — the fan-out has to happen here. Callers that act on project or
 * workspace state (new-thread defaults, a checkout, a worktree removal) use
 * this so every open "New Thread" composer on that project observes the
 * change, not just the one the user clicked in.
 */
export function forEachDraftPlaceholderPane(
  projectId: string,
  fn: (pane: ThreadPane) => void,
): void {
  if (!projectId) return;
  for (const pane of panes.values()) {
    if (!pane.hasDraftPlaceholder) continue;
    if (pane.thread?.projectId !== projectId) continue;
    fn(pane);
  }
}

export function destroyPane(id: string): void {
  const pane = panes.get(id);
  if (!pane) return;
  const order = orderedPaneIds();
  const removedIndex = order.indexOf(id);
  const nextFocusId = removedIndex > 0
    ? order[removedIndex - 1]
    : order.find((paneId) => paneId !== id) ?? null;
  pane.clear();
  panes = new Map(panes);
  panes.delete(id);
  paneActivationById = new Map(paneActivationById);
  paneActivationById.delete(id);
  removePaneLayoutItem(id, { persist: false });
  // Cascade: paired companion panes close with their source. Fired after the
  // source pane is fully torn down so observers see consistent registry/layout
  // state.
  for (const observer of paneDestroyedObservers) observer(id);
  // Focus fixup AFTER the cascade: focus may have pointed at this pane OR
  // at one of its just-closed companions — both leave a dangling id.
  if (focusedPaneId && !panes.has(focusedPaneId) && !hasLayoutPane(focusedPaneId)) {
    focusedPaneId = nextFocusId;
    if (nextFocusId) revealPane(nextFocusId);
  }
  requestPanePersistence();
}

export function closePanesShowingThread(threadId: string): void {
  const toDestroy: string[] = [];
  for (const pane of panes.values()) {
    if (pane.threadId === threadId) toDestroy.push(pane.paneId);
  }
  for (const id of toDestroy) destroyPane(id);
}

export function closePanesShowingThreads(threadIds: Iterable<string>): void {
  const idSet = new Set(threadIds);
  if (idSet.size === 0) return;
  const toDestroy: string[] = [];
  for (const pane of panes.values()) {
    if (pane.threadId && idSet.has(pane.threadId)) toDestroy.push(pane.paneId);
  }
  for (const id of toDestroy) destroyPane(id);
}

/**
 * Destroy the focused THREAD pane. A focused companion resolves to its
 * source thread pane here (via getFocusedPaneOrNull), so this closes the
 * thread — callers that want "close whatever holds focus, companion
 * included" must route through
 * companionPanes.svelte#closeFocusedPaneOrCompanion instead.
 */
export function closeFocusedPane(): void {
  const pane = getFocusedPaneOrNull();
  if (!pane) return;
  destroyPane(pane.paneId);
}

export function getPaneActivation(id: string): PaneActivation {
  return paneActivationById.get(id) ?? 'committed';
}

export function commitPanePreview(id: string): void {
  if (!panes.has(id)) return;
  if (paneActivationById.get(id) === 'committed') return;
  paneActivationById = new Map(paneActivationById).set(id, 'committed');
}

export function resetPanesForTest(): void {
  for (const pane of panes.values()) pane.clear();
  panes = new Map();
  paneActivationById = new Map();
  focusedPaneId = 'main';
  nextGeneratedPaneId = 1;
}

export function resetPaneRegistry(nextFocusedPaneId: string | null = null): void {
  for (const pane of panes.values()) pane.clear();
  panes = new Map();
  paneActivationById = new Map();
  focusedPaneId = nextFocusedPaneId;
  nextGeneratedPaneId = 1;
}

export async function hydrateRestoredPaneRegistry(
  entries: Array<{ paneId: string; thread: Thread }>,
  nextFocusedPaneId: string | null,
): Promise<void> {
  for (const pane of panes.values()) pane.clear();
  let nextPanes = new Map<string, ThreadPane>();
  let nextActivation = new Map<string, PaneActivation>();
  const hydratedPanes: Array<{ pane: ThreadPane; thread: Thread }> = [];
  // The other door into the registry. `paneLayoutPersistence` drops repeated
  // thread ids before it gets here; if one still arrives, the restore would
  // silently rebuild the two-panes-one-thread state the invariant forbids.
  const restoredThreadIds = new Set<string>();
  const droppedPaneIds = new Set<string>();
  for (const entry of entries) {
    if (restoredThreadIds.has(entry.thread.id)) {
      const detail = `thread ${entry.thread.id} requested for pane ${entry.paneId} and an earlier pane`;
      console.error(`[panes] duplicate thread in restore; pane skipped (${detail})`);
      reportFrontendDiagnostic(
        'panes: restore supplied one thread for two panes — the paneLayoutPersistence dedup '
        + 'let a duplicate through (frontend/CLAUDE.md → State ownership)',
        detail,
      );
      droppedPaneIds.add(entry.paneId);
      continue;
    }
    restoredThreadIds.add(entry.thread.id);
    const pane = createThreadPane({ paneId: entry.paneId });
    nextPanes = nextPanes.set(entry.paneId, pane);
    nextActivation = nextActivation.set(entry.paneId, 'committed');
    hydratedPanes.push({ pane, thread: entry.thread });
  }
  panes = nextPanes;
  paneActivationById = nextActivation;
  focusedPaneId = nextFocusedPaneId && panes.has(nextFocusedPaneId) ? nextFocusedPaneId : null;
  nextGeneratedPaneId = 1;
  const results = await Promise.allSettled(
    hydratedPanes.map(({ pane, thread }) => pane.switchThread(thread)),
  );
  for (const [index, result] of results.entries()) {
    if (result.status === 'fulfilled') continue;
    const paneId = hydratedPanes[index]?.pane.paneId ?? 'unknown';
    droppedPaneIds.add(paneId);
    console.error(`Failed to restore pane "${paneId}":`, result.reason);
  }
  if (droppedPaneIds.size === 0) return;
  // Skipped duplicates were never registered, so only their layout slot needs
  // clearing; the map deletes below are no-ops for them.
  for (const paneId of droppedPaneIds) {
    panes.get(paneId)?.clear();
    const nextPanes = new Map(panes);
    nextPanes.delete(paneId);
    panes = nextPanes;
    const nextActivation = new Map(paneActivationById);
    nextActivation.delete(paneId);
    paneActivationById = nextActivation;
    removePaneLayoutItem(paneId, { persist: false });
  }
  // Based on what was REQUESTED, not on what survived the resolve above: a
  // focused pane that was deduplicated never entered `panes`, so
  // `focusedPaneId` is already null here and a truthiness check would skip
  // the fallback entirely — leaving the session with no focused pane at all,
  // and every keyboard pane command a no-op until the user clicks one.
  if (
    nextFocusedPaneId !== null
    && (focusedPaneId === null || droppedPaneIds.has(focusedPaneId))
  ) {
    focusedPaneId = orderedPaneIds()[0] ?? null;
  }
}

export function findPaneShowingThread(threadId: string): ThreadPane | null {
  for (const pane of panes.values()) {
    if (pane.threadId !== threadId) continue;
    return pane;
  }
  return null;
}

/**
 * One thread is mounted in at most one pane. Enforced structurally by
 * `mountThreadInPane` being the only door into `replaceThreadInPane`; this is
 * the tripwire for a future path that mounts around it.
 *
 * Not dev-gated. It costs one scan of a registry holding a handful of panes,
 * on a user-initiated mount, and a breach produces two panes that disagree
 * about one thread — worth the evidence in the field, not just on a dev
 * machine. Constant message, ids in `detail`: an id in the message would mint
 * a diagnostic signature per thread and bypass the per-signature cap.
 */
function reportDuplicateMount(threadId: string, targetPaneId: string): void {
  for (const pane of panes.values()) {
    if (pane.paneId === targetPaneId || pane.threadId !== threadId) continue;
    const detail = `thread ${threadId} already in pane ${pane.paneId}, mounting into ${targetPaneId}`;
    console.error(`[panes] duplicate thread mount (${detail})`);
    reportFrontendDiagnostic(
      'panes: one thread mounted in two panes — an open bypassed mountThreadInPane '
      + '(frontend/CLAUDE.md → State ownership)',
      detail,
    );
    return;
  }
}

/**
 * Mount a thread into a pane unconditionally. PRIVATE: every open goes
 * through `mountThreadInPane`, which probes for an existing mount first.
 * Calling this directly is how a thread ends up in two panes.
 */
async function replaceThreadInPane(
  thread: Thread,
  targetPane: string | ThreadPane,
  activation: PaneActivation,
): Promise<ThreadPane> {
  const target = typeof targetPane === 'string'
    ? panes.get(targetPane)
    : targetPane;
  if (!target) {
    throw new Error(`Target pane "${targetPane}" is not registered.`);
  }
  reportDuplicateMount(thread.id, target.paneId);
  if (!panes.has(target.paneId)) {
    registerPane(target.paneId, target, activation);
  } else {
    paneActivationById = new Map(paneActivationById).set(target.paneId, activation);
  }
  addThreadPaneToLayout(target.paneId);
  focusedPaneId = target.paneId;
  revealPane(target.paneId);
  await target.switchThread(thread);
  requestPanePersistence();
  return target;
}

/**
 * Focus + reveal the pane already showing `threadId`, or null when none is.
 * A `committed` request also promotes a preview pane — opening a previewed
 * thread deliberately is what commits it.
 *
 * Synchronous on purpose: this runs ahead of every mount, and
 * `openTerminalThread` needs the empty-pane → terminal transition to stay
 * inside one paint frame (an await here reintroduces the "pick a project"
 * flash it was written to avoid).
 */
function revealThreadIfOpen(threadId: string, activation: PaneActivation): ThreadPane | null {
  const pane = findPaneShowingThread(threadId);
  if (!pane) return null;
  if (activation === 'committed') commitPanePreview(pane.paneId);
  focusedPaneId = pane.paneId;
  requestPanePersistence();
  revealPane(pane.paneId);
  return pane;
}

function resolveOpenTargetPane(targetPane?: string | ThreadPane | null): string | ThreadPane {
  if (targetPane) return targetPane;
  const focused = getFocusedPaneOrNull();
  if (focused) return focused;
  return ensureMainPane();
}

/**
 * THE way a thread is put on screen. Reveals the pane already showing it when
 * there is one — otherwise mounts it in `targetPane` (default: the focused
 * pane, else main). Every open path funnels here, which is what makes
 * "one thread, one pane" a property of the registry rather than a habit of
 * its callers.
 *
 * `activation` is the mount's intent, not just the new pane's state: a
 * `committed` open also promotes an existing preview pane.
 */
export async function mountThreadInPane(
  thread: Thread,
  targetPane?: string | ThreadPane | null,
  activation: PaneActivation = 'committed',
): Promise<ThreadPane> {
  const existing = revealThreadIfOpen(thread.id, activation);
  if (existing) return existing;
  return replaceThreadInPane(thread, resolveOpenTargetPane(targetPane), activation);
}

export async function openThreadInPane(
  thread: Thread,
  targetPane?: string | ThreadPane | null,
): Promise<ThreadPane> {
  return mountThreadInPane(thread, targetPane, 'committed');
}

export async function openThreadFromNavigation(
  thread: Thread,
  targetPane?: string | ThreadPane | null,
): Promise<ThreadPane> {
  return mountThreadInPane(thread, targetPane, 'preview');
}

export async function openThreadInNewPane(thread: Thread, insertIndex?: number): Promise<ThreadPane> {
  // Probed before minting the pane: a thread that is already open must not
  // leave an orphan empty pane behind.
  const existing = revealThreadIfOpen(thread.id, 'committed');
  if (existing) return existing;
  const paneId = nextPaneId();
  const pane = createPane(paneId);
  addThreadPaneToLayout(paneId, resolveNewPaneInsertIndex(insertIndex));
  return mountThreadInPane(thread, pane, 'committed');
}

/**
 * Create a brand-new pane committed to the layout and return it without
 * loading a thread. Callers populate it next — typically by calling
 * `pane.startDraftPlaceholder(project, mode)` for the "+ New Thread in
 * new pane" keyboard shortcut.
 */
export function openEmptyPane(insertIndex?: number): ThreadPane {
  const paneId = nextPaneId();
  const pane = createPane(paneId);
  addThreadPaneToLayout(paneId, resolveNewPaneInsertIndex(insertIndex));
  focusedPaneId = paneId;
  revealPane(paneId);
  requestPanePersistence();
  return pane;
}

export async function openThreadIdInNewPane(threadId: string, insertIndex?: number): Promise<ThreadPane | null> {
  // Probed before the registry lookup: an open thread is revealed even when
  // the sidebar registry no longer carries its row.
  const existing = revealThreadIfOpen(threadId, 'committed');
  if (existing) return existing;
  const thread = getThreadById(threadId);
  if (!thread) return null;
  return openThreadInNewPane(thread, insertIndex);
}

/**
 * Move logical focus one pane left/right in LAYOUT order — every mounted
 * pane is a stop, companions and take-control terminals included. Returns
 * the layout item now focused (callers branch on `kind` for follow-up like
 * composer/terminal DOM focus), or null at the strip's edge.
 */
export function focusAdjacentPane(direction: -1 | 1): PaneLayoutItem | null {
  const items = getPaneLayoutItems();
  const index = focusedPaneId
    ? items.findIndex((item) => item.paneId === focusedPaneId)
    : -1;
  if (index < 0) return null;
  const next = items[index + direction];
  if (!next) return null;
  focusedPaneId = next.paneId;
  requestPanePersistence();
  revealPane(next.paneId);
  return next;
}

export function moveFocusedPane(direction: -1 | 1): void {
  if (!focusedPaneId || !hasLayoutPane(focusedPaneId)) return;
  // A focused companion moves its whole [source + companions] block —
  // movePaneLayoutItem resolves the block from any member pane.
  movePaneLayoutItem(focusedPaneId, direction);
  revealPane(focusedPaneId);
}

/**
 * Apply a Thread update across every UI surface that holds it: the
 * global threads registry (sidebar list) AND every pane currently
 * displaying it.
 *
 * Use anywhere a binding response or local mutation produces a fresh
 * Thread that should be visible everywhere — model change, agent-mode
 * toggle, plan-comments-sent, branch switch, env change, discussion
 * start, worktree remove, etc.
 *
 * Replaces the dual-write `pane.replaceThread(t); replaceThread(t);`
 * pattern that was scattered across ~13 call sites. Forgetting one
 * half of the pair caused desync between sidebar list and chat header.
 *
 * Server-event handlers in `eventsThreadRows.ts` that need merge-aware
 * semantics (preserving local read markers / latest-completion
 * timestamps across server-pushed updates) keep using `syncThreadRow` —
 * that helper does `syncThread`'s fan-out plus the merge.
 *
 * `syncThread` deliberately does NOT bump project last-activity. The
 * backend's documented invariant (internal/store/threads.go) is that
 * in-place setters (model/branch/workspace/rename/...) do not write
 * threads.updated_at; real activity flows through
 * MarkThreadActivity at three sites (user_text persist, approval /
 * user-input request, turn complete). The frontend mirrors those
 * three sites via syncThreadActivity in eventsThreadRows.ts, which is the
 * legitimate sort-bump path. An unconditional touchProjectActivity
 * here would re-sort the project on every toolbar action.
 */
export function syncThread(thread: Thread): void {
  replaceThreadInRegistry(thread);
  for (const pane of panes.values()) {
    if (pane.threadId !== thread.id || !pane.thread) continue;
    pane.replaceThread(thread);
  }
}

// Arm the git-status store's pane bridge at module init. The import goes one
// way — panes → gitStatusStore — because the reverse closed a cycle
// (gitStatusStore → panes → thread → gitStatusStore) whose init order
// decided whether a module-level event listener registered. Importing this
// module is the whole wiring: no registration order, and no test reset that
// can leave branch reconciliation unable to reach a pane.
setGitStatusPaneBridge({
  syncThread,
  reportWorkspaceError(workspacePath, message) {
    // Only the panes still in that workspace. A pane that has moved on is
    // not shown an error about a checkout it left.
    for (const pane of panes.values()) {
      if (workspaceKeyForThread(pane.thread ?? null) !== workspacePath) continue;
      pane.setGeneralError(message);
    }
  },
});
