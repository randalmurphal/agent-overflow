/**
 * Per-tool diff sidebar state slot. Owns the active payload pointer,
 * the per-thread snapshot LRU, and the "restore on switch-back"
 * mechanic. Composed into the pane factory alongside the existing
 * `createDiffPanelState()` (the per-turn checkpoint drawer).
 *
 * Why this lives in its own module:
 *   - keeps ThreadPane focused on its core slices (items, turns,
 *     channel, etc.) and stops it from growing per-feature
 *   - makes the explicit deviation from Principle 4 ("frontend
 *     memory bounded by visible thread") self-contained: this module
 *     is the *only* place that holds state for non-visible threads,
 *     and the bound is named (DIFF_SIDEBAR_LRU_CAP, exported so
 *     tests pin it numerically)
 *   - mirrors the `diffPanel.svelte.ts` precedent so adding a fourth
 *     RHS panel is a matter of cloning this shape
 */

import type { DiffViewMode } from './diffPanel.svelte';

/**
 * Maximum number of per-thread sidebar snapshots held in memory.
 * Exported so tests can reference the cap symbolically and a memory
 * audit can grep one definition. The single explicit deviation from
 * frontend Principle 4 ("memory bounded by visible thread") is
 * scoped to this constant.
 */
export const DIFF_SIDEBAR_LRU_CAP = 20;

/** UI state of the per-tool diff sidebar that survives thread switches. */
export interface DiffSidebarUIState {
  viewMode: DiffViewMode;
  wordWrap: boolean;
  /** Ordered list of currently-expanded file paths. */
  expandedFiles: string[];
  scrollTop: number;
}

/** Pointer to which payload the sidebar is open on. */
export interface DiffSidebarPayload {
  payloadId: string;
  filePath?: string;
}

/** Snapshot saved to the per-thread map. */
export interface DiffSidebarSnapshot extends DiffSidebarUIState, DiffSidebarPayload {}

export interface DiffSidebarSlot {
  /** Open payload, or null when the sidebar is closed. */
  readonly activePayload: DiffSidebarPayload | null;
  /**
   * One-shot UI restore state pushed during thread switch.
   * `consumeRestore()` returns and clears it; the sidebar component
   * applies it on first mount.
   */
  readonly restoreState: DiffSidebarUIState | null;

  /** Open the sidebar on a payload. Clears any pending restore. */
  open(payload: DiffSidebarPayload): void;
  /**
   * Close the sidebar.
   * @param dropSnapshotForThreadId  When set, drop that thread's
   *   snapshot from the LRU. Used by explicit close ("user doesn't
   *   want to see it on switch-back"); pass `undefined` for mutex
   *   closes ("opening a different panel"), which preserve the
   *   snapshot for restoration.
   */
  close(dropSnapshotForThreadId?: string): void;

  /** Push current UI state up; bundled with payload on next switch. */
  recordUI(state: DiffSidebarUIState): void;
  /** Atomic take + clear. Sidebar calls this exactly once on mount. */
  consumeRestore(): DiffSidebarUIState | null;

  /**
   * Snapshot the outgoing thread's state during a thread switch.
   * No-op when the sidebar isn't open or no UI state has been recorded.
   * Always clears `activePayload` and the recorded UI.
   */
  snapshotForThread(prevThreadId: string): void;
  /**
   * Restore for the incoming thread (touch LRU). When a snapshot
   * exists, sets `activePayload` and seeds `restoreState`.
   */
  restoreForThread(nextThreadId: string): void;

  /** Wipe everything (used by pane.clear()). */
  reset(): void;

  /** Diagnostic — exposes the snapshot count for memory probes. */
  readonly snapshotCount: number;
}

export function createDiffSidebarSlot(): DiffSidebarSlot {
  const byThread = new Map<string, DiffSidebarSnapshot>();
  let activePayload: DiffSidebarPayload | null = $state(null);
  let restoreState: DiffSidebarUIState | null = $state(null);
  // Non-reactive `let`: the sidebar pushes its UI state via
  // recordUI() on every change, but no reader subscribes — the
  // value is only consumed at thread-switch boundaries, so making
  // it reactive would just trigger redundant effect re-runs.
  let currentUI: DiffSidebarUIState | null = null;

  function saveSnapshot(threadId: string, snapshot: DiffSidebarSnapshot): void {
    // Re-insert moves the entry to the end (most-recent slot) so the
    // Map's natural insertion order matches LRU order.
    byThread.delete(threadId);
    byThread.set(threadId, snapshot);
    while (byThread.size > DIFF_SIDEBAR_LRU_CAP) {
      const oldest = byThread.keys().next().value;
      if (!oldest) break;
      byThread.delete(oldest);
    }
  }

  return {
    get activePayload() { return activePayload; },
    get restoreState() { return restoreState; },
    get snapshotCount() { return byThread.size; },

    open(payload) {
      activePayload = payload;
      restoreState = null;
      currentUI = null;
    },

    close(dropSnapshotForThreadId) {
      if (dropSnapshotForThreadId) byThread.delete(dropSnapshotForThreadId);
      activePayload = null;
      restoreState = null;
      currentUI = null;
    },

    recordUI(state) {
      currentUI = state;
    },

    consumeRestore() {
      const state = restoreState;
      restoreState = null;
      return state;
    },

    snapshotForThread(prevThreadId) {
      if (activePayload && currentUI) {
        saveSnapshot(prevThreadId, {
          payloadId: activePayload.payloadId,
          filePath: activePayload.filePath,
          ...currentUI,
        });
      }
      activePayload = null;
      currentUI = null;
      restoreState = null;
    },

    restoreForThread(nextThreadId) {
      const restored = byThread.get(nextThreadId);
      if (!restored) return;
      // Touch LRU — re-restoring keeps the entry fresh.
      byThread.delete(nextThreadId);
      byThread.set(nextThreadId, restored);
      activePayload = { payloadId: restored.payloadId, filePath: restored.filePath };
      restoreState = {
        viewMode: restored.viewMode,
        wordWrap: restored.wordWrap,
        expandedFiles: restored.expandedFiles,
        scrollTop: restored.scrollTop,
      };
    },

    reset() {
      byThread.clear();
      activePayload = null;
      restoreState = null;
      currentUI = null;
    },
  };
}
