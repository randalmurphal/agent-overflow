import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  archivedChildrenKey,
  getArchivedChildren,
  holdArchivedChildren,
  releaseArchivedChildren,
  resetArchivedChildrenForTest,
  type ArchivedChildrenWant,
} from './agentArchivedChildren.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import type { ArchivedChildrenCursor } from './bindings';
import type { Thread } from '../types/models';

interface Page { threads: Thread[]; more: boolean }

function thread(id: string, updatedAt = 0): Thread {
  return {
    id, title: id, provider: 'claude', model: 'test', projectId: 'project-1', archived: true,
    projectPath: '/tmp/ws', workspacePath: '/tmp/ws', createdAt: 0, updatedAt,
  };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 6; i++) await Promise.resolve();
}

/** A computer holding `rows` newest first, answering pages as the store asks for them. */
function archive(rows: Thread[]) {
  return setBindingMock('ListArchivedSpawnedThreads', vi.fn(async (_id: string, _scopes: string[], after: ArchivedChildrenCursor, limit: number): Promise<Page> => {
    const from = after.id === '' ? 0 : rows.findIndex((row) => row.id === after.id) + 1;
    return { threads: rows.slice(from, from + limit), more: from + limit < rows.length };
  }));
}

const KEY = archivedChildrenKey('agent', 'ops@entry');
const want = (over: Partial<ArchivedChildrenWant> = {}): ArchivedChildrenWant => ({
  agentThreadId: 'agent', scopes: ['p1'], count: 2, needed: 8, ...over,
});

describe('agentArchivedChildren store', () => {
  const holder = {};
  beforeEach(() => {
    resetBindingMocks();
    resetArchivedChildrenForTest();
    resetToastsForTest();
  });

  it('reads one page per count for an open footer, by its container\'s scopes', async () => {
    const list = archive([thread('a'), thread('b')]);
    expect(getArchivedChildren(KEY)).toBeNull();
    holdArchivedChildren(holder, new Map([[KEY, want()]]));
    holdArchivedChildren(holder, new Map([[KEY, want()]]));
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(list.mock.calls[0].slice(0, 2)).toEqual(['agent', ['p1']]);
    expect(getArchivedChildren(KEY)).toEqual({ threads: [thread('a'), thread('b')], more: false });
    // The container's count moved: what was read no longer answers and a read goes out.
    holdArchivedChildren(holder, new Map([[KEY, want({ count: 3 })]]));
    expect(getArchivedChildren(KEY)).toBeNull();
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('reads only as far as the footer shows, and more as it shows more', async () => {
    const rows = Array.from({ length: 1000 }, (_, i) => thread(`t${String(i).padStart(4, '0')}`, 1000 - i));
    const list = archive(rows);
    holdArchivedChildren(holder, new Map([[KEY, want({ count: rows.length, needed: 8 })]]));
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(getArchivedChildren(KEY)?.threads).toHaveLength(20);
    expect(getArchivedChildren(KEY)?.more).toBe(true);
    // Show More, twice: the next pages follow the last row read.
    holdArchivedChildren(holder, new Map([[KEY, want({ count: rows.length, needed: 48 })]]));
    await flush();
    const read = getArchivedChildren(KEY)!.threads;
    expect(read.map((row) => row.id)).toEqual(rows.slice(0, 48).map((row) => row.id));
    expect(list.mock.calls.map((call) => [(call[2] as ArchivedChildrenCursor).id, call[3]])).toEqual([['', 20], ['t0019', 28]]);
  });

  it('drops what a closed footer read, and reads afresh when it opens again', async () => {
    const list = archive([thread('a')]);
    holdArchivedChildren(holder, new Map([[KEY, want({ count: 1 })]]));
    await flush();
    holdArchivedChildren(holder, new Map());
    expect(getArchivedChildren(KEY)).toBeNull();
    holdArchivedChildren(holder, new Map([[KEY, want({ count: 1 })]]));
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
    // A footer another list holds open stays.
    const other = {};
    holdArchivedChildren(other, new Map([[KEY, want({ count: 1 })]]));
    releaseArchivedChildren(holder);
    expect(getArchivedChildren(KEY)?.threads).toHaveLength(1);
    releaseArchivedChildren(other);
    expect(getArchivedChildren(KEY)).toBeNull();
  });

  it('lets only the newest read of a footer answer, across a close and reopen at the same count', async () => {
    const answers: ((page: Page) => void)[] = [];
    setBindingMock('ListArchivedSpawnedThreads', () => new Promise<Page>((resolve) => { answers.push(resolve); }));
    holdArchivedChildren(holder, new Map([[KEY, want()]]));
    holdArchivedChildren(holder, new Map());
    holdArchivedChildren(holder, new Map([[KEY, want()]]));
    expect(answers).toHaveLength(2);
    answers[0]({ threads: [thread('old')], more: false });
    await flush();
    expect(getArchivedChildren(KEY)).toBeNull();
    answers[1]({ threads: [thread('new-1'), thread('new-2')], more: false });
    await flush();
    expect(getArchivedChildren(KEY)?.threads.map((row) => row.id)).toEqual(['new-1', 'new-2']);
  });

  it('drops a page read for a count the row no longer says', async () => {
    const answers: ((page: Page) => void)[] = [];
    setBindingMock('ListArchivedSpawnedThreads', () => new Promise<Page>((resolve) => { answers.push(resolve); }));
    holdArchivedChildren(holder, new Map([[KEY, want({ count: 1 })]]));
    holdArchivedChildren(holder, new Map([[KEY, want({ count: 2 })]]));
    answers[0]({ threads: [thread('a')], more: false });
    await flush();
    expect(getArchivedChildren(KEY)).toBeNull();
    answers[1]({ threads: [thread('a'), thread('b')], more: false });
    await flush();
    expect(getArchivedChildren(KEY)?.threads).toHaveLength(2);
  });

  it('reports a failed read with a toast and reads again on the next hold', async () => {
    const list = setBindingMock('ListArchivedSpawnedThreads', vi.fn(async () => { throw new Error('backend down'); }));
    vi.spyOn(console, 'error').mockImplementation(() => {});
    holdArchivedChildren(holder, new Map([[KEY, want({ count: 1 })]]));
    await flush();
    expect(getArchivedChildren(KEY)).toBeNull();
    expect(getToasts().some((t) => t.type === 'error' && t.message.includes('finished threads'))).toBe(true);
    list.mockImplementation(async () => ({ threads: [thread('a')], more: false }));
    holdArchivedChildren(holder, new Map([[KEY, want({ count: 1 })]]));
    await flush();
    expect(getArchivedChildren(KEY)?.threads).toHaveLength(1);
  });
});
