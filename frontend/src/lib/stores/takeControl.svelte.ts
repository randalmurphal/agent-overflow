// Take-control pane registry + lifecycle.
//
// A take-control pane is a terminal mirror of a claude-tui session's PTY,
// opened to the immediate right of the thread pane that hosts that session
// (its "source pane"). It is NOT a ThreadPane: it has no thread row, no
// timeline, no composer, and is never persisted. The user model is "one entity
// across two panes":
//
//   - Open from the source pane's header button; it appears to the right.
//   - Close the source pane           → the take-control pane closes too.
//   - Close the take-control pane      → the source thread pane stays.
//   - Switch the source pane's thread  → the terminal follows (re-attaches to
//                                        the new thread, or closes if the new
//                                        thread isn't a live claude-tui session).
//   - The provider session ends        → the take-control pane closes.
//
// This store owns only the PAIRING (which take-control pane belongs to which
// source pane). The mirrored threadId is resolved reactively from the source
// pane by TakeControlPane.svelte, so "switch follows" needs no bookkeeping
// here. Adjacency ("never a dangling pane on the side") is enforced
// structurally by resnapTakeControlItems in paneLayout.svelte.ts; cascade close
// is wired through panes.svelte.ts's destroy observer.

import {
  addPaneLayoutItem,
  averagePaneRatio,
  getPaneLayoutItems,
  removePaneLayoutItem,
} from './paneLayout.svelte';
import { getPane, setPaneDestroyedObserver } from './panes.svelte';

export interface TakeControlPaneState {
  // The take-control pane's own id (a layout paneId, e.g. "take-control-main").
  paneId: string;
  // The source thread pane this terminal mirror is paired to.
  sourcePaneId: string;
}

// Keyed by the take-control pane's own paneId.
let takeControlPanes: Map<string, TakeControlPaneState> = $state(new Map());

function takeControlPaneIdFor(sourcePaneId: string): string {
  return `take-control-${sourcePaneId}`;
}

/**
 * The take-control pane currently paired to `sourcePaneId`, or null. Reactive:
 * the header button reads this for its pressed state.
 */
export function takeControlForSource(sourcePaneId: string): TakeControlPaneState | null {
  for (const state of takeControlPanes.values()) {
    if (state.sourcePaneId === sourcePaneId) return state;
  }
  return null;
}

/** Runtime state for a take-control pane id, or null. Read by PaneHost. */
export function getTakeControlPane(paneId: string): TakeControlPaneState | null {
  return takeControlPanes.get(paneId) ?? null;
}

function registerTakeControlPane(state: TakeControlPaneState): void {
  takeControlPanes = new Map(takeControlPanes).set(state.paneId, state);
}

function unregisterTakeControlPane(paneId: string): void {
  if (!takeControlPanes.has(paneId)) return;
  takeControlPanes = new Map(takeControlPanes);
  takeControlPanes.delete(paneId);
}

/**
 * Open a take-control pane to the right of `sourcePaneId`. Idempotent: if one
 * is already paired to that source, returns it without opening a second.
 * Callers gate on the source pane hosting a claude-tui thread; this only needs
 * the source pane to exist in the layout.
 */
export function openTakeControl(sourcePaneId: string): TakeControlPaneState | null {
  const existing = takeControlForSource(sourcePaneId);
  if (existing) return existing;

  const layoutItems = getPaneLayoutItems();
  const sourceIndex = layoutItems.findIndex((item) => item.paneId === sourcePaneId);
  if (sourceIndex < 0) return null;

  const paneId = takeControlPaneIdFor(sourcePaneId);
  const sourceRatio = layoutItems[sourceIndex].ratio;
  // Take-control panes are ephemeral, so their layout item is never persisted
  // (buildSnapshot skips any layout item with no ThreadPane). persist:false
  // keeps the open from scheduling a settings write for an unpersistable item.
  addPaneLayoutItem(
    {
      id: paneId,
      paneId,
      kind: 'take-control',
      ratio: sourceRatio > 0 ? sourceRatio : averagePaneRatio(),
      sourcePaneId,
    },
    sourceIndex + 1,
    { persist: false },
  );
  const state: TakeControlPaneState = { paneId, sourcePaneId };
  registerTakeControlPane(state);
  return state;
}

/** Close a take-control pane by its own paneId. The source pane is untouched. */
export function closeTakeControl(paneId: string): void {
  if (!takeControlPanes.has(paneId)) return;
  unregisterTakeControlPane(paneId);
  removePaneLayoutItem(paneId, { persist: false });
}

/**
 * Toggle the take-control pane for a source pane: open if absent, close if
 * present. Backs the header button. Returns the resulting open state.
 */
export function toggleTakeControl(sourcePaneId: string): boolean {
  const existing = takeControlForSource(sourcePaneId);
  if (existing) {
    closeTakeControl(existing.paneId);
    return false;
  }
  return openTakeControl(sourcePaneId) !== null;
}

/** True when a take-control pane is open for `sourcePaneId`. */
export function isTakeControlOpen(sourcePaneId: string): boolean {
  return takeControlForSource(sourcePaneId) !== null;
}

/**
 * Cascade hook: a source thread pane was destroyed, so close any take-control
 * pane paired to it. Registered as the pane-destroyed observer by
 * installTakeControl. Also defensively closes a take-control pane that is
 * itself the destroyed pane (no-op in practice — take-control panes aren't
 * ThreadPanes, so destroyPane never targets them).
 */
function onSourcePaneDestroyed(destroyedPaneId: string): void {
  const paired = takeControlForSource(destroyedPaneId);
  if (paired) closeTakeControl(paired.paneId);
  if (takeControlPanes.has(destroyedPaneId)) closeTakeControl(destroyedPaneId);
}

/**
 * Close a take-control pane whose source pane no longer hosts a live
 * claude-tui session. Called by TakeControlPane.svelte when the source pane is
 * gone, switched to a non-claude-tui thread, or the provider session died.
 */
export function closeTakeControlForLostSource(paneId: string): void {
  closeTakeControl(paneId);
}

/**
 * Resolve the source pane a take-control pane is paired to, for reactive reads
 * in the component. Returns null if the take-control pane or its source pane is
 * gone.
 */
export function sourcePaneForTakeControl(paneId: string) {
  const state = takeControlPanes.get(paneId);
  if (!state) return null;
  return getPane(state.sourcePaneId) ?? null;
}

/** Wire the cascade-close observer. Call once during app store setup. */
export function installTakeControl(): void {
  setPaneDestroyedObserver(onSourcePaneDestroyed);
}

export function resetTakeControlForTest(): void {
  takeControlPanes = new Map();
  setPaneDestroyedObserver(null);
}
