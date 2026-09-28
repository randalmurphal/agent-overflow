// Companion pane registry + lifecycle for every pane paired to a source
// thread pane: plan, review, browser, take-control, and side-chat.
//
// Companions are snapped immediately to the source pane's right by
// paneLayout.svelte.ts, and they belong to the THREAD the source pane was
// showing when they opened. When the pane leaves that thread they are
// hidden, not closed: companionStash.ts remembers them for the thread and
// reopens them when it is shown again. Only an explicit close forgets one.
//
// take-control renders its own raw PTY surface, and side-chat an ordinary
// thread pane over its own scratch thread, which can have companions of its
// own. take-control, browser and side-chat are ephemeral: browser follows a
// live Chrome target that cannot be restored, and a side chat's thread is
// deleted when it is closed and swept at boot. The other kinds are persisted
// and restored by paneLayoutPersistence.ts.

import {
  addPaneLayoutItem,
  averagePaneWidthPx,
  getPaneLayoutItems,
  isCompanionKind,
  removePaneLayoutItem,
  type CompanionPaneKind,
} from './paneLayout.svelte';
import { isCompactLayout, onScreenCompactPaneId } from './layoutMode.svelte';
import {
  addPaneDestroyedObserver,
  closeFocusedPane,
  destroyPane,
  focusPane,
  getFocusedPaneId,
  getPane,
  revealPane,
} from './panes.svelte';
import { deleteSideChatThread } from './sideChatThread';

export type CompanionKind = CompanionPaneKind;
/**
 * The kinds CompanionPane hosts as panel bodies. take-control and side-chat
 * render whole surfaces of their own through PaneHost's dedicated branches.
 */
export type CompanionPanelKind = Exclude<CompanionKind, 'take-control' | 'side-chat'>;
export type PersistedCompanionKind = Exclude<
  CompanionKind,
  'take-control' | 'browser' | 'side-chat'
>;

function isEphemeralCompanionKind(kind: CompanionKind): boolean {
  return kind === 'take-control' || kind === 'browser' || kind === 'side-chat';
}

/**
 * A side chat is the one companion that owns a ThreadPane of its own (the
 * registry entry under its companion pane id), so its close goes through
 * destroyPane and takes its scratch thread with it.
 */
function isThreadHostingCompanionKind(kind: CompanionKind): boolean {
  return kind === 'side-chat';
}

export interface CompanionPaneState {
  paneId: string;
  kind: CompanionKind;
  sourcePaneId: string;
}

let companionPanes: Map<string, CompanionPaneState> = $state(new Map());
let unsubscribePaneDestroyed: (() => void) | null = null;

function companionPaneIdFor(sourcePaneId: string, kind: CompanionKind): string {
  return `${kind}-${sourcePaneId}`;
}

export function companionForSource(
  sourcePaneId: string,
  kind: CompanionKind,
): CompanionPaneState | null {
  for (const state of companionPanes.values()) {
    if (state.sourcePaneId === sourcePaneId && state.kind === kind) return state;
  }
  return null;
}

export function getCompanionPane(paneId: string): CompanionPaneState | null {
  return companionPanes.get(paneId) ?? null;
}

function registerCompanionPane(state: CompanionPaneState): void {
  companionPanes = new Map(companionPanes).set(state.paneId, state);
}

function unregisterCompanionPane(paneId: string): void {
  if (!companionPanes.has(paneId)) return;
  companionPanes = new Map(companionPanes);
  companionPanes.delete(paneId);
}

// After the source and everything hanging from it, including the companions
// of a side chat among its companions.
function companionInsertIndex(sourcePaneId: string): number {
  const layoutItems = getPaneLayoutItems();
  const sourceIndex = layoutItems.findIndex((item) => item.paneId === sourcePaneId);
  if (sourceIndex < 0) return -1;
  const descendants = new Set([sourcePaneId]);
  let insertIndex = sourceIndex + 1;
  for (let i = sourceIndex + 1; i < layoutItems.length; i += 1) {
    const item = layoutItems[i];
    if (!isCompanionKind(item.kind) || !item.sourcePaneId || !descendants.has(item.sourcePaneId)) break;
    descendants.add(item.paneId);
    insertIndex = i + 1;
  }
  return insertIndex;
}

/** The companions paired to `sourcePaneId`, in layout order. */
export function companionsForSource(sourcePaneId: string): CompanionPaneState[] {
  const out: CompanionPaneState[] = [];
  for (const item of getPaneLayoutItems()) {
    const state = companionPanes.get(item.paneId);
    if (state && state.sourcePaneId === sourcePaneId) out.push(state);
  }
  return out;
}

export interface OpenCompanionOptions {
  /** Width to open at. Defaults to the source pane's. */
  widthPx?: number;
  /**
   * Scroll the new companion into view. On by default, since opening is
   * explicit intent; a companion reopened with its thread is not, and must
   * not pull a compact screen off the thread.
   */
  reveal?: boolean;
}

export function openCompanion(
  sourcePaneId: string,
  kind: CompanionKind,
  options: OpenCompanionOptions = {},
): CompanionPaneState | null {
  const existing = companionForSource(sourcePaneId, kind);
  if (existing) return existing;

  const layoutItems = getPaneLayoutItems();
  const sourceIndex = layoutItems.findIndex((item) => item.paneId === sourcePaneId);
  if (sourceIndex < 0) return null;

  const paneId = companionPaneIdFor(sourcePaneId, kind);
  const sourceWidthPx = layoutItems[sourceIndex].widthPx;
  const widthPx = options.widthPx ?? sourceWidthPx;
  addPaneLayoutItem(
    {
      id: paneId,
      paneId,
      kind,
      widthPx: widthPx > 0 ? widthPx : averagePaneWidthPx(),
      sourcePaneId,
    },
    // take-control hugs its source even past open panel companions: the
    // shared top-border indicator reads the two panes as one entity, so
    // nothing may sit between them. Panel companions append after the
    // source's existing companion run.
    kind === 'take-control' ? sourceIndex + 1 : companionInsertIndex(sourcePaneId),
    // Ephemeral companions are skipped by buildSnapshot, so opening one
    // must not schedule a settings write it can't contribute to.
    { persist: !isEphemeralCompanionKind(kind) },
  );
  const state: CompanionPaneState = { paneId, kind, sourcePaneId };
  registerCompanionPane(state);
  // Focus deliberately stays on the source thread — the user opts into the
  // companion by clicking or pane-navigating into it.
  if (options.reveal ?? true) revealPane(paneId);
  return state;
}

/**
 * Close a companion for good: a side chat's thread is deleted, and so is
 * anything the side chat itself had open.
 */
export function closeCompanion(paneId: string): void {
  takeDownCompanion(paneId, 'close');
}

/**
 * Take a companion down because its source pane is leaving the thread it
 * belongs to. Unlike a close, a side chat keeps its thread: companionStash.ts
 * has recorded it and reopens it with the thread. A side chat's pane goes
 * through destroyPane, whose close edge stashes the side chat's own
 * companions under the side chat's thread.
 */
export function hideCompanion(paneId: string): void {
  takeDownCompanion(paneId, 'hide');
}

function takeDownCompanion(paneId: string, mode: 'close' | 'hide'): void {
  const state = companionPanes.get(paneId);
  if (!state) return;
  // Read before the section leaves the DOM: under compact the strip shows
  // one pane, and closing the one on screen must bring its thread back
  // rather than glide to whichever sibling companion is left.
  const wasOnScreen = isCompactLayout() && onScreenCompactPaneId() === paneId;
  if (isThreadHostingCompanionKind(state.kind) && mode === 'close') {
    // First, while the side chat still exists: its own companions close
    // with it rather than being remembered for a thread about to be deleted.
    closeCompanionsForSource(paneId);
  }
  unregisterCompanionPane(paneId);
  if (isThreadHostingCompanionKind(state.kind)) {
    // Read before the registry entry goes. destroyPane removes the layout
    // item itself.
    const threadId = getPane(paneId)?.threadId ?? '';
    destroyPane(paneId);
    if (mode === 'close') void deleteSideChatThread(threadId);
  } else {
    removePaneLayoutItem(paneId, { persist: !isEphemeralCompanionKind(state.kind) });
  }
  // A focused companion hands focus back to its source. During a source-pane
  // destroy cascade the source is already gone — focusPane no-ops on the
  // missing id and destroyPane's own dangling-focus fixup takes over.
  if (getFocusedPaneId() === paneId) focusPane(state.sourcePaneId);
  if (wasOnScreen) revealPane(state.sourcePaneId);
}

/**
 * Pane-scoped close for whatever holds focus. A focused companion closes
 * ITSELF (never the thread it's paired to — closeCompanion hands focus
 * back to the source); anything else destroys the focused thread pane.
 * This is the single owner of the companion-vs-thread close branch —
 * `pane.close` routes here. It lives in this store because
 * panes.svelte.ts must not depend on companion stores, while the reverse
 * dependency is the sanctioned direction.
 */
export function closeFocusedPaneOrCompanion(): void {
  const focusedId = getFocusedPaneId();
  const companion = focusedId ? companionPanes.get(focusedId) : null;
  if (companion) closeCompanion(companion.paneId);
  else closeFocusedPane();
}

/**
 * Close one pane by id, whichever kind it is. The pane-header close control
 * uses it so a companion closes as a companion (its registration dropped,
 * its source refocused, a side chat's thread deleted) rather than as a bare
 * pane destroy.
 */
export function closePaneById(paneId: string): void {
  const companion = companionPanes.get(paneId);
  if (companion) closeCompanion(companion.paneId);
  else destroyPane(paneId);
}

export function toggleCompanion(sourcePaneId: string, kind: CompanionKind): boolean {
  const existing = companionForSource(sourcePaneId, kind);
  if (existing) {
    closeCompanion(existing.paneId);
    return false;
  }
  return openCompanion(sourcePaneId, kind) !== null;
}

export function isCompanionOpen(sourcePaneId: string, kind: CompanionKind): boolean {
  return companionForSource(sourcePaneId, kind) !== null;
}

export function restoreCompanion(
  sourcePaneId: string,
  kind: PersistedCompanionKind,
  paneId: string,
): CompanionPaneState {
  const state: CompanionPaneState = { paneId, kind, sourcePaneId };
  registerCompanionPane(state);
  return state;
}

/**
 * Close every companion paired to `sourcePaneId` for good. The leaving-a-
 * thread edges stash companions instead (companionStash.ts); this is for a
 * pane cleared without leaving to anywhere (registry resets, a deleted
 * thread) and for the destroyed-pane cascade below.
 */
export function closeCompanionsForSource(sourcePaneId: string): void {
  const paneIds = Array.from(companionPanes.values())
    .filter((state) => state.sourcePaneId === sourcePaneId)
    .map((state) => state.paneId);
  for (const paneId of paneIds) closeCompanion(paneId);
}

// Source panes and side chats both arrive here: a side chat IS a ThreadPane,
// so destroyPane can name one directly (its thread was deleted elsewhere, or
// Keep replaced the pane). Dropping its registration first keeps a stale
// entry out of the registry; the cascade below is then the ordinary
// source-pane case.
function onPaneDestroyed(destroyedPaneId: string): void {
  unregisterCompanionPane(destroyedPaneId);
  closeCompanionsForSource(destroyedPaneId);
}

export function installCompanionPanes(): void {
  unsubscribePaneDestroyed?.();
  unsubscribePaneDestroyed = addPaneDestroyedObserver(onPaneDestroyed);
}

export function resetCompanionPanesForTest(): void {
  companionPanes = new Map();
  unsubscribePaneDestroyed?.();
  unsubscribePaneDestroyed = null;
}
