import { beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  getArchivedChildren,
  loadArchivedChildren,
  resetArchivedChildrenForTest,
} from './agentArchivedChildren.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import type { Thread } from '../types/models';

function thread(id: string): Thread {
  return {
    id, title: id, provider: 'claude', model: 'test', projectId: 'project-1', archived: true,
    projectPath: '/tmp/ws', workspacePath: '/tmp/ws', createdAt: 0, updatedAt: 0,
  };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 4; i++) await Promise.resolve();
}

describe('agentArchivedChildren store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetArchivedChildrenForTest();
    resetToastsForTest();
  });

  it('reads the list once per count and answers only for the count it was read for', async () => {
    const list = setBindingMock('ListArchivedSpawnedThreads', async () => [thread('a'), thread('b')]);
    expect(getArchivedChildren('agent', 2)).toBeNull();
    loadArchivedChildren('agent', 2);
    loadArchivedChildren('agent', 2);
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(list).toHaveBeenCalledWith('agent');
    expect(getArchivedChildren('agent', 2)?.map((t) => t.id)).toEqual(['a', 'b']);
    // The row's count moved: the old list no longer answers and a read goes out.
    expect(getArchivedChildren('agent', 3)).toBeNull();
    loadArchivedChildren('agent', 3);
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
    expect(getArchivedChildren('agent', 3)).toHaveLength(2);
  });

  it('drops a stale answer when the count moved while the read was out', async () => {
    let resolveFirst: (rows: Thread[]) => void = () => {};
    const list = setBindingMock('ListArchivedSpawnedThreads', () => new Promise<Thread[]>((resolve) => { resolveFirst = resolve; }));
    loadArchivedChildren('agent', 1);
    await flush();
    list.mockImplementation(async () => [thread('a'), thread('b')]);
    loadArchivedChildren('agent', 2);
    resolveFirst([thread('a')]);
    await flush();
    expect(getArchivedChildren('agent', 1)).toBeNull();
    expect(getArchivedChildren('agent', 2)?.map((t) => t.id)).toEqual(['a', 'b']);
  });

  it('forgets the list when the count reaches zero', async () => {
    setBindingMock('ListArchivedSpawnedThreads', async () => [thread('a')]);
    loadArchivedChildren('agent', 1);
    await flush();
    expect(getArchivedChildren('agent', 1)).toHaveLength(1);
    loadArchivedChildren('agent', 0);
    expect(getArchivedChildren('agent', 1)).toBeNull();
  });

  it('reports a failed read with a toast and lets the footer ask again', async () => {
    const list = setBindingMock('ListArchivedSpawnedThreads', async () => { throw new Error('backend down'); });
    loadArchivedChildren('agent', 1);
    await flush();
    expect(getArchivedChildren('agent', 1)).toBeNull();
    expect(getToasts().some((t) => t.type === 'error' && t.message.includes('finished threads'))).toBe(true);
    list.mockImplementation(async () => [thread('a')]);
    loadArchivedChildren('agent', 1);
    await flush();
    expect(getArchivedChildren('agent', 1)).toHaveLength(1);
  });
});
