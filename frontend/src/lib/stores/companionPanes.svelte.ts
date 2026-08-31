// Companion pane registry + lifecycle for every pane paired to a source
// thread pane: plan, review, browser, and take-control.
//
// Companions are snapped immediately to the source pane's right by
// paneLayout.svelte.ts, and they belong to the THREAD the source pane was
// showing when they opened — ThreadPane closes them (closeCompanionsForSource)
// whenever that thread changes.
//
// take-control renders its own raw PTY surface. It and browser are ephemeral;
// browser follows a live Chrome target that cannot be restored. The other
// kinds are persisted and restored by paneLayoutPersistence.ts.

import {
  addPaneLayoutItem,
  averagePaneWidthPx,
  getPaneLayoutItems,
  isCompanionKind,
  removePaneLayoutItem,
  type CompanionPaneKind,
} from './paneLayout.svelte';
import {
  addPaneDestroyedObserver,
  closeFocusedPane,
  focusPane,
  getFocusedPaneId,
  revealPane,
} from './panes.svelte';

export type CompanionKind = CompanionPaneKind;
/** The kinds CompanionPane hosts as panel bodies (everything but take-control). */
export type CompanionPanelKind = Exclude<CompanionKind, 'take-control'>;
export type PersistedCompanionKind = Exclude<CompanionKind, 'take-control' | 'browser'>;

function isEphemeralCompanionKind(kind: CompanionKind): boolean {
  return kind === 'take-control' || kind === 'browser';
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

function companionInsertIndex(sourcePaneId: string): number {
  const layoutItems = getPaneLayoutItems();
  const sourceIndex = layoutItems.findIndex((item) => item.paneId === sourcePaneId);
  if (sourceIndex < 0) return -1;
  let insertIndex = sourceIndex + 1;
  for (let i = sourceIndex + 1; i < layoutItems.length; i += 1) {
    const item = layoutItems[i];
    if (!isCompanionKind(item.kind) || item.sourcePaneId !== sourcePaneId) break;
    insertIndex = i + 1;
  }
  return insertIndex;
}

export function openCompanion(
  sourcePaneId: string,
  kind: CompanionKind,
): CompanionPaneState | null {
  const existing = companionForSource(sourcePaneId, kind);
  if (existing) return existing;

  const layoutItems = getPaneLayoutItems();
  const sourceIndex = layoutItems.findIndex((item) => item.paneId === sourcePaneId);
  if (sourceIndex < 0) return null;

  const paneId = companionPaneIdFor(sourcePaneId, kind);
  const sourceWidthPx = layoutItems[sourceIndex].widthPx;
  addPaneLayoutItem(
    {
      id: paneId,
      paneId,
      kind,
      widthPx: sourceWidthPx > 0 ? sourceWidthPx : averagePaneWidthPx(),
      sourcePaneId,
    },
    // take-control hugs its source even past open panel companions: the
    // shared top-border indicator reads the two panes as one entity, so
    // nothing may sit between them. Panel companions append after the
    // source's existing companion run.
    kind === 'take-control' ? sourceIndex + 1 : companionInsertIndex(sourcePaneId),
    // take-control is ephemeral — buildSnapshot skips it, so opening one
    // must not schedule a settings write it can't contribute to.
    { persist: !isEphemeralCompanionKind(kind) },
  );
  const state: CompanionPaneState = { paneId, kind, sourcePaneId };
  registerCompanionPane(state);
  // Opening is explicit intent: scroll the new companion into view. Focus
  // deliberately stays on the source thread — the user opts into the
  // companion by clicking or pane-navigating into it.
  revealPane(paneId);
  return state;
}

export function closeCompanion(paneId: string): void {
  const state = companionPanes.get(paneId);
  if (!state) return;
  unregisterCompanionPane(paneId);
  removePaneLayoutItem(paneId, { persist: !isEphemeralCompanionKind(state.kind) });
  // A focused companion hands focus back to its source. During a source-pane
  // destroy cascade the source is already gone — focusPane no-ops on the
  // missing id and destroyPane's own dangling-focus fixup takes over.
  if (getFocusedPaneId() === paneId) focusPane(state.sourcePaneId);
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
 * Close every companion paired to `sourcePaneId`. Companions belong to
 * the thread they were opened for, not the pane slot: ThreadPane calls
 * this when its thread changes (switch, clear, draft start), and the
 * destroyed-pane cascade below funnels through it too.
 */
export function closeCompanionsForSource(sourcePaneId: string): void {
  const paneIds = Array.from(companionPanes.values())
    .filter((state) => state.sourcePaneId === sourcePaneId)
    .map((state) => state.paneId);
  for (const paneId of paneIds) closeCompanion(paneId);
}

// Only source panes can arrive here: companions are not ThreadPanes, so
// destroyPane never targets them (same invariant takeControl relies on).
function onSourcePaneDestroyed(destroyedPaneId: string): void {
  closeCompanionsForSource(destroyedPaneId);
}

export function installCompanionPanes(): void {
  unsubscribePaneDestroyed?.();
  unsubscribePaneDestroyed = addPaneDestroyedObserver(onSourcePaneDestroyed);
}

export function resetCompanionPanesForTest(): void {
  companionPanes = new Map();
  unsubscribePaneDestroyed?.();
  unsubscribePaneDestroyed = null;
}
