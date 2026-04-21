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
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    ...overrides,
  };
}

export function installPaneMocks(items: Item[] = []): void {
  setBindingMock('SwitchThread', async () => {});
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
