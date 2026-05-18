import type { Thread } from '../types/models';
import type { ActiveOptionSet, DesignViewport } from '../types/design';
import type { DiffViewMode } from './diffPanel.svelte';
import { getPaneWidth } from './layoutMetrics.svelte';
import { syncThread } from './panes.svelte';
import type { ThreadPane } from './thread.svelte';

export const RHS_PANEL_LRU_CAP = 20;
export const RHS_PANEL_DEFAULT_WIDTH = 560;
export const RHS_PANEL_MIN_WIDTH = 380;

// Keep this much of the owning pane available for the chat column when
// a right-side panel is open.
export const RHS_PANEL_CHAT_RESERVE_WIDTH = 500;

export type RhsPanel =
  | { kind: 'plan' }
  | { kind: 'design-preview' }
  | { kind: 'diff-checkpoint' }
  | { kind: 'diff-payload'; payloadId: string; filePath?: string };

/**
 * Narrow projection of a ThreadPane handed to RHS panel bodies. Bodies
 * receive this instead of `pane: ThreadPane` so they cannot accidentally
 * couple to chat-only state (`pane.items`, `pane.timelineRevision`,
 * streaming flags) — coupling that, in PlanSidebar, was the structural
 * cause of body-blank flicker during chat streaming.
 *
 * The shell (RhsSidebarShell) keeps the full pane because it owns the
 * resizer and snapshot/restore chrome. Only panel bodies are narrowed.
 *
 * Adding a new panel kind that needs row registries (expansion state,
 * attachment cache, subagent group flags) should EXTEND this interface
 * with the specific accessors it needs, not widen back to `pane`.
 */
export interface PanelContext {
  /** Thread this panel belongs to. May be null when the pane has no thread
   *  loaded — bodies should treat null as "nothing to render" rather than
   *  rendering a degraded surface. The shell remounts the body on thread
   *  switch via its `{#key}` boundary. */
  threadId: string | null;
  /** Current thread object for panel actions that need thread metadata. */
  thread: Thread | null;
  /** Stable identifier for the owning pane (today: 'main'). Plumbed for
   *  the multi-pane / tiling future where each pane has its own sidebar. */
  paneId: string;
  /** Workspace root for resolving relative paths in panel content. */
  workspacePath: string | undefined;
  /** Design preview viewport selected for this pane. */
  designViewport: DesignViewport;
  /** Active design option set, when the agent has emitted choices. */
  activeOptionSet: ActiveOptionSet | null;
  /** Close this panel (X button, ESC, programmatic). Generic across panel
   *  kinds — wires through pane.closeRhsPanel(). */
  close(): void;
  /** Sync an updated thread to every UI surface holding it (the owning
   *  pane plus the global threads registry). Use after a panel action
   *  mutates the thread server-side. */
  replaceThread(thread: Thread): void;
  /** Switch this pane to another thread after a panel action creates one. */
  switchThread(thread: Thread): Promise<void>;
  /** Update the design preview viewport. */
  setDesignViewport(viewport: DesignViewport): void;
  /** Activate or clear the design option set. */
  setActiveOptionSet(set: ActiveOptionSet | null): void;
  /** Rehydrate design options from the backend workdir. */
  refreshDesignOptions(threadId: string): Promise<void>;
}

/**
 * Builds a stable PanelContext for the given pane.
 *
 * `threadId` and `workspacePath` are exposed as GETTERS so the returned
 * object's identity is stable across `pane.thread` reassignments (which
 * happen on every turn_started, turn_completed, usage event, and
 * thread:updated event — many per chat message). Reactive consumers
 * reading `ctx.threadId` still pick up changes via the getter.
 *
 * Without getter-based stability, the factory ran inside RhsSidebarShell's
 * `$derived(makePanelContext(pane))` and re-created the ctx object on
 * every pane.thread reassignment, propagating prop-identity churn into
 * PlanSidebar and visibly flickering the sidebar surface.
 */
export function makePanelContext(pane: ThreadPane): PanelContext {
  return {
    paneId: pane.paneId,
    get threadId() { return pane.threadId; },
    get thread() { return pane.thread; },
    get workspacePath() { return pane.thread?.workspacePath; },
    get designViewport() { return pane.designViewport; },
    get activeOptionSet() { return pane.activeOptionSet; },
    close: () => pane.closeRhsPanel(),
    replaceThread: syncThread,
    switchThread: (thread) => pane.switchThread(thread),
    setDesignViewport: (viewport) => pane.setDesignViewport(viewport),
    setActiveOptionSet: (set) => pane.setActiveOptionSet(set),
    refreshDesignOptions: (threadId) => pane.applyDesignOptionsUpdate(threadId, ''),
  };
}

export interface DiffSidebarUIState {
  viewMode: DiffViewMode;
  wordWrap: boolean;
  expandedFiles: string[];
  scrollTop: number;
}

interface RhsPanelSnapshot {
  width: number;
  panel: RhsPanel | null;
  diffPayloadUI: DiffSidebarUIState | null;
}

export interface RhsPanelSlot {
  readonly activePanel: RhsPanel | null;
  readonly width: number;
  readonly diffPayloadRestoreState: DiffSidebarUIState | null;
  readonly snapshotCount: number;

  open(panel: RhsPanel): void;
  closeForThread(threadId?: string): void;
  setWidthLive(next: number): void;
  persistWidthForThread(threadId?: string): void;
  getMaxWidth(): number;
  recordDiffPayloadUI(state: DiffSidebarUIState): void;
  consumeDiffPayloadRestore(): DiffSidebarUIState | null;
  snapshotForThread(threadId: string): void;
  restoreForThread(threadId: string): void;
  reset(): void;
}

function maxWidthForPane(paneId: string): number {
  return Math.max(RHS_PANEL_MIN_WIDTH, getPaneWidth(paneId) - RHS_PANEL_CHAT_RESERVE_WIDTH);
}

function clampWidth(paneId: string, px: number): number {
  return Math.max(RHS_PANEL_MIN_WIDTH, Math.min(maxWidthForPane(paneId), Math.round(px)));
}

function clonePanel(panel: RhsPanel): RhsPanel {
  if (panel.kind !== 'diff-payload') return { kind: panel.kind };
  return { kind: 'diff-payload', payloadId: panel.payloadId, filePath: panel.filePath };
}

function cloneDiffUI(state: DiffSidebarUIState | null): DiffSidebarUIState | null {
  if (!state) return null;
  return {
    viewMode: state.viewMode,
    wordWrap: state.wordWrap,
    expandedFiles: state.expandedFiles.slice(),
    scrollTop: state.scrollTop,
  };
}

export function createRhsPanelSlot(paneId: string): RhsPanelSlot {
  const byThread = new Map<string, RhsPanelSnapshot>();
  let activePanel: RhsPanel | null = $state(null);
  let width = $state(RHS_PANEL_DEFAULT_WIDTH);
  let diffPayloadRestoreState: DiffSidebarUIState | null = $state(null);
  let currentDiffPayloadUI: DiffSidebarUIState | null = null;

  function saveSnapshot(threadId: string, snapshot: RhsPanelSnapshot): void {
    byThread.delete(threadId);
    byThread.set(threadId, snapshot);
    while (byThread.size > RHS_PANEL_LRU_CAP) {
      const oldest = byThread.keys().next().value;
      if (!oldest) break;
      byThread.delete(oldest);
    }
  }

  function currentSnapshot(panel: RhsPanel | null): RhsPanelSnapshot {
    const diffPayloadUI = panel?.kind === 'diff-payload'
      ? cloneDiffUI(currentDiffPayloadUI)
      : null;
    return {
      width,
      panel: panel ? clonePanel(panel) : null,
      diffPayloadUI,
    };
  }

  return {
    get activePanel() { return activePanel; },
    get width() { return clampWidth(paneId, width); },
    get diffPayloadRestoreState() { return diffPayloadRestoreState; },
    get snapshotCount() { return byThread.size; },

    open(panel) {
      activePanel = clonePanel(panel);
      diffPayloadRestoreState = null;
      currentDiffPayloadUI = null;
    },

    closeForThread(threadId) {
      if (threadId) {
        saveSnapshot(threadId, currentSnapshot(null));
      }
      activePanel = null;
      diffPayloadRestoreState = null;
      currentDiffPayloadUI = null;
    },

    setWidthLive(next) {
      const clamped = clampWidth(paneId, next);
      if (clamped === width) return;
      width = clamped;
    },

    persistWidthForThread(threadId) {
      if (!threadId) return;
      const existing = byThread.get(threadId);
      saveSnapshot(threadId, {
        width,
        panel: existing?.panel ? clonePanel(existing.panel) : activePanel ? clonePanel(activePanel) : null,
        diffPayloadUI: cloneDiffUI(currentDiffPayloadUI ?? existing?.diffPayloadUI ?? null),
      });
    },

    getMaxWidth() {
      return maxWidthForPane(paneId);
    },

    recordDiffPayloadUI(state) {
      if (activePanel?.kind !== 'diff-payload') return;
      currentDiffPayloadUI = cloneDiffUI(state);
    },

    consumeDiffPayloadRestore() {
      const state = diffPayloadRestoreState;
      diffPayloadRestoreState = null;
      return cloneDiffUI(state);
    },

    snapshotForThread(threadId) {
      saveSnapshot(threadId, currentSnapshot(activePanel));
      activePanel = null;
      diffPayloadRestoreState = null;
      currentDiffPayloadUI = null;
    },

    restoreForThread(threadId) {
      const restored = byThread.get(threadId);
      if (!restored) {
        activePanel = null;
        width = RHS_PANEL_DEFAULT_WIDTH;
        diffPayloadRestoreState = null;
        currentDiffPayloadUI = null;
        return;
      }
      byThread.delete(threadId);
      byThread.set(threadId, restored);
      width = clampWidth(paneId, restored.width);
      activePanel = restored.panel ? clonePanel(restored.panel) : null;
      diffPayloadRestoreState = activePanel?.kind === 'diff-payload'
        ? cloneDiffUI(restored.diffPayloadUI)
        : null;
      currentDiffPayloadUI = cloneDiffUI(restored.diffPayloadUI);
    },

    reset() {
      byThread.clear();
      activePanel = null;
      width = RHS_PANEL_DEFAULT_WIDTH;
      diffPayloadRestoreState = null;
      currentDiffPayloadUI = null;
    },
  };
}
