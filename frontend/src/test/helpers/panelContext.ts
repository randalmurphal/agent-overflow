// A hand-built PanelContext for panel-body tests that have no real
// ThreadPane to project from.
//
// Extracted so the interface can grow without every such test breaking: a
// body that needs a member to DO something overrides it, and the defaults
// are inert answers ("nothing loaded", "no-op"), never plausible fakes.
// Tests that DO have a pane should call `makePanelContext(pane, close)`
// instead — this is only for the ones that don't.

import type { PanelContext } from '../../lib/stores/panelContext.svelte';
import { registry as activityRunRegistry } from './activityRuns';

export function makeStubPanelContext(overrides: Partial<PanelContext> = {}): PanelContext {
  return {
    paneId: 'source-pane',
    threadId: 'thread-1',
    thread: null,
    workspacePath: '/repo',
    designViewport: 'desktop',
    activeOptionSet: null,
    items: [],
    timelineRevision: 0,
    getItemById: () => undefined,
    ensureSubagentChildren: async () => false,
    pendingApprovals: [],
    activityRuns: activityRunRegistry(),
    latestSettledTurn: null,
    canCompose: false,
    requestScrollToItem() {},
    openAgentScope() {},
    closeAgentPane() {},
    close() {},
    replaceThread() {},
    async switchThread() {},
    setDesignViewport() {},
    setActiveOptionSet() {},
    async refreshDesignOptions() {},
    ...overrides,
  };
}
