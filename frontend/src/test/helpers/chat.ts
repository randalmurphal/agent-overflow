import { createThreadPane, type ThreadPane } from '../../lib/stores/thread.svelte';
import type { Item, Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';

export function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test thread',
    provider: 'claude',
    workspacePath: '/tmp/workspace',
    projectPath: '/tmp/workspace',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

export function makeItem(overrides: Partial<Item> = {}): Item {
  const createdAt = overrides.createdAt ?? 0;
  return {
    id: 'item-1',
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: 'hello',
    highlightedContent: '',
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    ...overrides,
  };
}

export function installPaneMocks(items: Item[] = []): void {
  setBindingMock('SwitchThread', async () => {});
  // pane.switchThread auto-marks the thread as read; default both
  // read-state bindings to no-ops so component tests that don't care
  // don't have to stub them.
  setBindingMock('MarkThreadRead', async () => {});
  setBindingMock('MarkThreadUnread', async () => {});
  // The pane now loads the tail of history via ListRecentThreadItems on
  // switch; ListItems remains mocked because a few component tests (and
  // a couple of integration fixtures) still reach for it directly.
  // Both default to the same items array so helpers and raw mocks
  // behave consistently.
  setBindingMock('ListRecentThreadItems', async () => ({
    items,
    oldestTurnIndex: items.length > 0 ? items[0].turnIndex : -1,
    hasMore: false,
  }));
  setBindingMock('ListItems', async () => items);
  // Empty turn history by default. Tests that want to exercise rehydration
  // override this via setBindingMock('ListRecentTurns', ...) after calling
  // buildPane / installPaneMocks.
  setBindingMock('ListRecentTurns', async () => []);
}

export async function buildPane(
  thread: Thread = makeThread(),
  items: Item[] = [],
): Promise<ThreadPane> {
  installPaneMocks(items);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  return pane;
}
