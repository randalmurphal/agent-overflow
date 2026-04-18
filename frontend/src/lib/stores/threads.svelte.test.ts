import { describe, expect, it, vi } from 'vitest';
import {
  getThreads,
  refreshThreads,
  prependThread,
  removeThread,
  updateThreadTitle,
  updateThreadModel,
  replaceThread,
} from './threads.svelte';
import type { Thread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';

function makeThread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id,
    title: `Thread ${id}`,
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('threads store', () => {
  describe('refreshThreads()', () => {
    it('replaces the store with the RPC result', async () => {
      const loaded = [makeThread('t1'), makeThread('t2')];
      setBindingMock('ListThreads', async () => loaded);
      await refreshThreads();
      expect(getThreads().map((t) => t.id)).toEqual(['t1', 't2']);
    });

    it('leaves the store alone on failure (logs + toast)', async () => {
      // Seed known state.
      setBindingMock('ListThreads', async () => [makeThread('keep')]);
      await refreshThreads();
      expect(getThreads().map((t) => t.id)).toEqual(['keep']);

      // Failing RPC.
      setBindingMock('ListThreads', async () => { throw new Error('rpc down'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
      await refreshThreads();
      // Prior value preserved.
      expect(getThreads().map((t) => t.id)).toEqual(['keep']);
      consoleErr.mockRestore();
    });
  });

  describe('mutations', () => {
    it('prependThread puts new thread at the head', async () => {
      setBindingMock('ListThreads', async () => [makeThread('b'), makeThread('c')]);
      await refreshThreads();
      prependThread(makeThread('a'));
      expect(getThreads().map((t) => t.id)).toEqual(['a', 'b', 'c']);
    });

    it('removeThread drops the matching id and leaves others', async () => {
      setBindingMock('ListThreads', async () => [
        makeThread('a'),
        makeThread('b'),
        makeThread('c'),
      ]);
      await refreshThreads();
      removeThread('b');
      expect(getThreads().map((t) => t.id)).toEqual(['a', 'c']);
    });

    it('removeThread on missing id is a no-op', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a')]);
      await refreshThreads();
      removeThread('missing');
      expect(getThreads().map((t) => t.id)).toEqual(['a']);
    });

    it('updateThreadTitle replaces only the matching thread title', async () => {
      setBindingMock('ListThreads', async () => [
        makeThread('a', { title: 'A' }),
        makeThread('b', { title: 'B' }),
      ]);
      await refreshThreads();
      updateThreadTitle('a', 'renamed');

      const found = getThreads().find((t) => t.id === 'a');
      expect(found?.title).toBe('renamed');
      // Other thread untouched.
      expect(getThreads().find((t) => t.id === 'b')?.title).toBe('B');
    });

    it('updateThreadModel replaces only the matching thread model', async () => {
      setBindingMock('ListThreads', async () => [
        makeThread('a', { model: 'sonnet' }),
        makeThread('b', { model: 'sonnet' }),
      ]);
      await refreshThreads();
      updateThreadModel('b', 'opus');

      expect(getThreads().find((t) => t.id === 'a')?.model).toBe('sonnet');
      expect(getThreads().find((t) => t.id === 'b')?.model).toBe('opus');
    });

    it('replaceThread swaps the thread object wholesale', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a', { title: 'old' })]);
      await refreshThreads();
      replaceThread(makeThread('a', { title: 'new', model: 'gpt-5' }));

      const found = getThreads().find((t) => t.id === 'a');
      expect(found?.title).toBe('new');
      expect(found?.model).toBe('gpt-5');
    });

    it('replaceThread is a no-op when id does not exist', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a')]);
      await refreshThreads();
      replaceThread(makeThread('missing', { title: 'x' }));
      expect(getThreads()).toHaveLength(1);
      expect(getThreads()[0].id).toBe('a');
    });
  });
});
