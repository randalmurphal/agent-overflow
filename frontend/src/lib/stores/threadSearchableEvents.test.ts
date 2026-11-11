// A find over a thread or agent scope re-reads when the transcript's text
// changes, including rows the loaded window refuses or does not hold. The
// pane counts those events while a find watches and retains none of them.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createAgentScopeView, type AgentScopeView } from './agentScopeView.svelte';
import { createThreadPane, type ThreadPane } from './thread.svelte';
import { installPaneMocks, installTimelineScopeCapability, makeItem, makeThread } from '../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { applyItemStreamEvent, flushItemEventQueue } from './eventsItemStream';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { cursorFromItem } from './threadItems';
import type { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import type { ItemStreamEvent } from '../types/events';

const threadId = 'find-thread';
const agent = makeItem({ id: 'agent', threadId, turnIndex: 0, kind: 'tool_call', toolName: 'Agent', status: 'running' });

function send(evt: ItemStreamEvent): void {
  applyItemStreamEvent(evt);
  flushItemEventQueue();
}

/** Every searchable event for `id`, a row the window does not hold. */
function unheldEvents(id: string, parentId: string | undefined, turnIndex: number): ItemStreamEvent[] {
  return [
    { action: 'upsert', threadId, item: makeItem({ id, threadId, parentId, turnIndex, kind: 'assistant_text', status: 'streaming', summary: 'needle' }) },
    { action: 'delta', threadId, itemId: id, parentId, kind: 'assistant_text', delta: ' more needle', updatedAt: 3 },
    { action: 'patch', threadId, itemId: id, parentId, kind: 'assistant_text', patch: { rev: 4, summary: 'needle done', status: 'completed', updatedAt: 4 } },
    { action: 'remove', threadId, itemId: id },
  ];
}

function expectCounted(surface: ThreadPane, events: ItemStreamEvent[], held: string[]): void {
  // Closed find: the events change nothing a find reads.
  let revision = surface.searchRevision;
  for (const evt of events) send(evt);
  expect(surface.searchRevision).toBe(revision);

  const stop = surface.watchSearchableEvents();
  for (const evt of events) {
    revision = surface.searchRevision;
    send(evt);
    expect(surface.searchRevision, evt.action).toBeGreaterThan(revision);
  }
  expect(surface.items.map((item) => item.id)).toEqual(held);

  // A repeated stop releases only its own watch.
  const other = surface.watchSearchableEvents();
  stop();
  stop();
  revision = surface.searchRevision;
  send(events[0]);
  expect(surface.searchRevision).toBeGreaterThan(revision);
  other();
  revision = surface.searchRevision;
  for (const evt of events) send(evt);
  expect(surface.searchRevision).toBe(revision);
}

const views: AgentScopeView[] = [];
let warn: ReturnType<typeof vi.spyOn>;
beforeEach(() => {
  installTimelineScopeCapability();
  resetBindingMocks();
  warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => {
  for (const view of views.splice(0)) view.dispose();
  resetPanesForTest();
  warn.mockRestore();
});

describe('searchable events beyond the loaded window', () => {
  it('advance a watched thread pane\'s search revision without entering its window', async () => {
    const held = [makeItem({ id: 'a', threadId, turnIndex: 1 }), makeItem({ id: 'b', threadId, turnIndex: 2 })];
    installPaneMocks(held);
    const slice = getBindingMock('ListThreadSliceAround')!;
    setBindingMock('ListThreadSliceAround', async (...args: unknown[]) => ({ ...(await slice(...args) as PagedItems), hasMoreNewer: true }));
    const pane = createThreadPane({ paneId: 'main' });
    registerPaneForTest('main', pane);
    await pane.switchThread(makeThread({ id: threadId }));
    expect(pane.hasMoreNewer).toBe(true);

    expectCounted(pane, unheldEvents('beyond', undefined, 9), ['a', 'b']);
  });

  it('advance a watched agent scope\'s search revision without entering its window', async () => {
    installPaneMocks([agent]);
    const pane = createThreadPane({ paneId: 'main' });
    registerPaneForTest('main', pane);
    await pane.switchThread(makeThread({ id: threadId }));
    const child = makeItem({ id: 'child', threadId, parentId: agent.id, turnIndex: 0, itemIndex: 1 });
    const page: PagedItems = {
      items: [child], runs: [], scope: { root: agent, lifecycle: agent }, hasMore: false,
      hasMoreOlder: false, hasMoreNewer: true, oldestTurnIndex: 0, newestTurnIndex: 0,
      oldestCursor: { ...cursorFromItem(child), itemId: child.id }, newestCursor: { ...cursorFromItem(child), itemId: child.id },
    } as PagedItems;
    setBindingMock('SyncThreadWindow', async () => ({ status: 'stale', page }));
    const view = createAgentScopeView(pane, agent.id, { viewKey: 'agent', openAgentPane: vi.fn() });
    views.push(view);
    view.start();
    await vi.waitFor(() => expect(view.pane.loading).toBe(false));
    expect(view.pane.hasMoreNewer).toBe(true);

    expectCounted(view.pane, unheldEvents('later', agent.id, 9), ['child']);
  });
});
