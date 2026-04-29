import type { DiffViewMode } from './diffPanel.svelte';

export const RHS_PANEL_LRU_CAP = 20;
export const RHS_PANEL_DEFAULT_WIDTH = 560;
export const RHS_PANEL_MIN_WIDTH = 380;

const DEFAULT_MAIN_RESERVE = 640;

export type RhsPanel =
  | { kind: 'plan' }
  | { kind: 'diff-checkpoint' }
  | { kind: 'diff-payload'; payloadId: string; filePath?: string };

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

function getMaxWidth(): number {
  if (typeof window === 'undefined') return Number.POSITIVE_INFINITY;
  return Math.max(RHS_PANEL_MIN_WIDTH, window.innerWidth - DEFAULT_MAIN_RESERVE);
}

function clampWidth(px: number): number {
  return Math.max(RHS_PANEL_MIN_WIDTH, Math.min(getMaxWidth(), Math.round(px)));
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

export function createRhsPanelSlot(): RhsPanelSlot {
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
    get width() { return width; },
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
      const clamped = clampWidth(next);
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

    getMaxWidth,

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
      width = clampWidth(restored.width);
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
