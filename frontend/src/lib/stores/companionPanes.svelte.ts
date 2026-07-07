// Plan/design/review companion pane registry + lifecycle.
//
// These panes are paired to a source thread pane and snapped immediately to
// its right by paneLayout.svelte.ts. Unlike take-control, they are persisted
// and restored by paneLayoutPersistence.ts.

import {
  addPaneLayoutItem,
  averagePaneRatio,
  getPaneLayoutItems,
  isCompanionKind,
  removePaneLayoutItem,
} from './paneLayout.svelte';
import { addPaneDestroyedObserver } from './panes.svelte';

export type CompanionKind = 'plan' | 'design-preview' | 'review';

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
  const sourceRatio = layoutItems[sourceIndex].ratio;
  addPaneLayoutItem(
    {
      id: paneId,
      paneId,
      kind,
      ratio: sourceRatio > 0 ? sourceRatio : averagePaneRatio(),
      sourcePaneId,
    },
    companionInsertIndex(sourcePaneId),
  );
  const state: CompanionPaneState = { paneId, kind, sourcePaneId };
  registerCompanionPane(state);
  return state;
}

export function closeCompanion(paneId: string): void {
  if (!companionPanes.has(paneId)) return;
  unregisterCompanionPane(paneId);
  removePaneLayoutItem(paneId);
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
  kind: CompanionKind,
  paneId: string,
): CompanionPaneState {
  const state: CompanionPaneState = { paneId, kind, sourcePaneId };
  registerCompanionPane(state);
  return state;
}

// Only source panes can arrive here: companions are not ThreadPanes, so
// destroyPane never targets them (same invariant takeControl relies on).
function onSourcePaneDestroyed(destroyedPaneId: string): void {
  const paneIds = Array.from(companionPanes.values())
    .filter((state) => state.sourcePaneId === destroyedPaneId)
    .map((state) => state.paneId);
  for (const paneId of paneIds) closeCompanion(paneId);
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
