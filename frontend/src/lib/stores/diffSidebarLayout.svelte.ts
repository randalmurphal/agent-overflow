// Diff sidebar width — composes the shared RHS layout factory.
// Storage key + default + min are tuned for per-tool diff inspection
// (wider than plan review because patch hunks need horizontal room).
// All resize behavior lives in `rhsSidebarLayout.svelte.ts`; this file
// just supplies the panel-specific config and re-exports the API
// under stable names so existing callers and tests keep working.

import { createRhsSidebarLayout } from './rhsSidebarLayout.svelte';

const layout = createRhsSidebarLayout({
  storageKey: 'agent-overflow:diff-sidebar:width',
  defaultWidth: 480,
  minWidth: 360,
});

export const DIFF_SIDEBAR_MIN_WIDTH = layout.minWidth;
export const getDiffSidebarWidth = layout.getWidth;
export const getDiffSidebarMaxWidth = layout.getMaxWidth;
export const setDiffSidebarWidthLive = layout.setWidthLive;
export const persistDiffSidebarWidth = layout.persistWidth;
export const setDiffSidebarWidth = layout.setWidth;
export const readPersistedDiffSidebarWidth = layout.readPersistedWidth;
export const resetDiffSidebarLayoutForTest = layout.resetForTest;
