// Plan sidebar width — composes the shared RHS layout factory.
// Storage key + default + min are tuned for plan review (narrower
// than the diff sidebar; plan items are mostly text). All resize
// behavior lives in `rhsSidebarLayout.svelte.ts`; this file just
// supplies the panel-specific config and re-exports the API under
// stable names so existing callers and tests keep working.

import { createRhsSidebarLayout } from './rhsSidebarLayout.svelte';

const layout = createRhsSidebarLayout({
  storageKey: 'agent-overflow:plan-sidebar:width',
  defaultWidth: 440,
  minWidth: 320,
});

export const PLAN_SIDEBAR_MIN_WIDTH = layout.minWidth;
export const getPlanSidebarWidth = layout.getWidth;
export const getPlanSidebarMaxWidth = layout.getMaxWidth;
export const setPlanSidebarWidthLive = layout.setWidthLive;
export const persistPlanSidebarWidth = layout.persistWidth;
export const setPlanSidebarWidth = layout.setWidth;
export const readPersistedPlanSidebarWidth = layout.readPersistedWidth;
export const resetPlanSidebarLayoutForTest = layout.resetForTest;
