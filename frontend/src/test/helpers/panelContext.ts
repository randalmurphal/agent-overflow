// A hand-built PanelContext for panel-body tests that have no real
// ThreadPane to project from.
//
// Extracted so the interface can grow without every such test breaking: a
// body that needs a member to DO something overrides it, and the defaults
// are inert answers ("nothing loaded", "no-op"), never plausible fakes.
// Tests that DO have a pane should call `makePanelContext(pane, close)`
// instead — this is only for the ones that don't.

import type { PanelContext } from '../../lib/stores/panelContext.svelte';
import type { Thread } from '../../lib/types/models';

export function makeStubPanelContext(overrides: Partial<PanelContext> = {}): PanelContext {
  return {
    paneId: 'source-pane',
    threadId: 'thread-1',
    // Consistent with `threadId`: a context that names a thread has the row.
    // The review pane keys its state on `thread.id` rather than `threadId`,
    // because a draft placeholder has the former and not the latter.
    thread: { id: 'thread-1', projectId: 'project-1', workspacePath: '/repo' } as Thread,
    workspacePath: '/repo',
    workspace: { projectId: 'project-1', workspacePath: '/repo' },
    items: [],
    timelineRevision: 0,
    getItemById: () => undefined,
    ensureSubagentChildren: async () => false,
    closeAgentPane() {},
    close() {},
    replaceThread() {},
    async switchThread() {},
    ...overrides,
  };
}
