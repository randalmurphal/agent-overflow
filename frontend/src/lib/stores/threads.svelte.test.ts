import { describe, expect, it, vi } from 'vitest';
import {
  getThreads,
  getThreadLiveActivityAt,
  refreshThreads,
  replaceAllThreads,
  prependThread,
  prependThreadReply,
  removeThread,
  updateThreadTitle,
  updateThreadModel,
  replaceThread,
  touchThreadActivity,
} from './threads.svelte';
import type { Thread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { applyThreadUpdated } from './eventsThreadRows';
import { threadRowFrameMark } from './threadRowFrames';

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

  describe('rows as values', () => {
    it('rejects a write into a stored row or into the list', async () => {
      setBindingMock('ListThreads', async () => [makeThread('t1'), makeThread('t2')]);
      await refreshThreads();
      updateThreadTitle('t2', 'Renamed');
      const rows = getThreads() as Thread[];
      // Both a row a read committed and a row a mutation replaced.
      expect(() => { rows[0].title = 'written in place'; }).toThrow(TypeError);
      expect(() => { rows[1].title = 'written in place'; }).toThrow(TypeError);
      expect(() => { rows.push(makeThread('t3')); }).toThrow(TypeError);
      expect(getThreads().map((t) => t.title)).toEqual([makeThread('t1').title, 'Renamed']);
    });

    it('stores its own copy of a reactive row a pane hands over', async () => {
      setBindingMock('ListThreads', async () => [makeThread('t1')]);
      await refreshThreads();
      const paneRow = $state(makeThread('draft'));

      prependThread(paneRow);
      expect(getThreads().map((t) => t.id)).toEqual(['draft', 't1']);
      expect(getThreads()[0]).not.toBe(paneRow);
      expect(getThreads()[0]).toEqual(makeThread('draft'));

      // The pane writing to its row does not reach the catalog's copy.
      paneRow.title = 'typed in the pane';
      expect(getThreads()[0].title).toBe(makeThread('draft').title);

      replaceThread(paneRow);
      expect(getThreads()[0]).not.toBe(paneRow);
      expect(getThreads()[0].title).toBe('typed in the pane');

      // Rows the store already holds keep their identity across a change.
      const kept = getThreads()[1];
      updateThreadTitle('draft', 'renamed');
      expect(getThreads()[1]).toBe(kept);
    });

    it('keeps its own list when a caller hands over an array', async () => {
      setBindingMock('ListThreads', async () => [makeThread('t1'), makeThread('t2')]);
      await refreshThreads();
      // Rows the store already holds, in the caller's array.
      const rows = [...getThreads()];
      replaceAllThreads(rows);
      expect(getThreads()).not.toBe(rows);
      expect(getThreads()[0]).toBe(rows[0]);

      // The caller's array stays the caller's: writable, and not the list.
      rows.pop();
      expect(getThreads().map((t) => t.id)).toEqual(['t1', 't2']);

      const empty: Thread[] = [];
      replaceAllThreads(empty);
      empty.push(makeThread('late'));
      expect(getThreads()).toEqual([]);
    });
  });

  describe('mutations', () => {
    it('prependThread puts new thread at the head', async () => {
      setBindingMock('ListThreads', async () => [makeThread('b'), makeThread('c')]);
      await refreshThreads();
      prependThread(makeThread('a'));
      expect(getThreads().map((t) => t.id)).toEqual(['a', 'b', 'c']);
    });

    // A create's reply is read before its `listed` echo leaves, and a
    // later write's frame can land before the reply too.
    it('prependThreadReply keeps a frame that arrived during the call', async () => {
      setBindingMock('ListThreads', async () => [makeThread('b')]);
      await refreshThreads();
      const mark = threadRowFrameMark();
      applyThreadUpdated({ action: 'listed', thread: makeThread('fresh', { title: 'Named meanwhile' }) });
      const held = prependThreadReply(makeThread('fresh'), mark);
      expect(held.title).toBe('Named meanwhile');
      expect(getThreads().map((t) => [t.id, t.title])).toEqual([['fresh', 'Named meanwhile'], ['b', 'Thread b']]);
      const later = threadRowFrameMark();
      expect(prependThreadReply(makeThread('fresh', { title: 'Reply' }), later).title).toBe('Reply');
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

    it('touchThreadActivity bumps the live box and leaves the array signal silent', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a', { updatedAt: 100 })]);
      await refreshThreads();
      const before = getThreads();
      const rowBefore = before[0];

      const touched = touchThreadActivity('a', 500);

      expect(touched?.id).toBe('a');
      // Tripwire for the per-beat sidebar re-render class (2026-08-26):
      // an activity bump is a field patch and must NOT mint a new
      // threads array or row object — that identity churn is what made
      // every streaming flush reconcile the sidebar's animated
      // each-blocks (FLIP measure = forced layout per beat).
      expect(getThreads()).toBe(before);
      expect(getThreads()[0]).toBe(rowBefore);
      expect(getThreads()[0].updatedAt).toBe(100);
      expect(getThreadLiveActivityAt(rowBefore)).toBe(500);
    });

    it('touchThreadActivity ignores stale timestamps and missing threads', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a', { updatedAt: 500 })]);
      await refreshThreads();

      const stale = touchThreadActivity('a', 100);
      const missing = touchThreadActivity('missing', 900);
      const invalid = touchThreadActivity('a', Number.NaN);

      expect(stale?.updatedAt).toBe(500);
      expect(missing).toBeUndefined();
      expect(invalid).toBeUndefined();
      expect(getThreadLiveActivityAt(getThreads()[0])).toBe(500);
    });

    it('removeThread drops the live-activity box so a recycled id starts clean', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a', { updatedAt: 100 })]);
      await refreshThreads();
      touchThreadActivity('a', 900);
      removeThread('a');

      prependThread(makeThread('a', { updatedAt: 200 }));
      expect(getThreadLiveActivityAt(getThreads()[0])).toBe(200);
    });
  });
});
