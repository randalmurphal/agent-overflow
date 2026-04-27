// Checkpoint diff panel width — composes the shared RHS layout
// factory. Tuned for full-checkpoint diff inspection (slightly wider
// than the per-tool diff sidebar because checkpoint diffs span many
// files). Behavior matches every other RHS panel; per-panel config
// is the storage key + default + min only.

import { createRhsSidebarLayout } from './rhsSidebarLayout.svelte';

const layout = createRhsSidebarLayout({
  storageKey: 'agent-overflow:diff-panel:width',
  defaultWidth: 600,
  minWidth: 380,
});

export const DIFF_PANEL_MIN_WIDTH = layout.minWidth;
export const getDiffPanelWidth = layout.getWidth;
export const getDiffPanelMaxWidth = layout.getMaxWidth;
export const setDiffPanelWidthLive = layout.setWidthLive;
export const persistDiffPanelWidth = layout.persistWidth;
export const setDiffPanelWidth = layout.setWidth;
export const readPersistedDiffPanelWidth = layout.readPersistedWidth;
export const resetDiffPanelLayoutForTest = layout.resetForTest;
