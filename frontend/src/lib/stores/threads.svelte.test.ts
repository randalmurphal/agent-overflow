import { describe, expect, it, vi } from 'vitest';
import {
  getThreads,
  getThreadLiveActivityAt,
  refreshThreads,
  prependThread,
  removeThread,
  updateThreadTitle,
  updateThreadModel,
  replaceThread,
  touchThreadActivity,
} from './threads.svelte';
import type { Thread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  isAgentDoneExpanded,
  isAgentExpanded,
  resetSidebarForTest,
  setAgentExpanded,
  toggleAgentDone,
} from './sidebar.svelte';
import {
  getArchivedChildren,
  loadArchivedChildren,
  resetArchivedChildrenForTest,
} from './agentArchivedChildren.svelte';
import {
  ensureAgentWakes,
  getAgentWakeRows,
  resetAgentWakesForTest,
} from './agentWakes.svelte';

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

// A thread that stops being an assistant's current thread in the list takes
// its own lists with it, whichever way it left; the assistant's fold and
// footer, keyed by the assistant, go only when no thread of it is left.
describe('standing agent state on removal', () => {
  const agent = (promoted: boolean) => makeThread('agent', promoted ? { agentId: 'agent', archivedChildCount: 1 } : {});

  async function settle(): Promise<void> {
    for (let i = 0; i < 6; i += 1) await Promise.resolve();
  }

  async function primeAgentState(): Promise<void> {
    resetSidebarForTest();
    resetArchivedChildrenForTest();
    resetAgentWakesForTest();
    setBindingMock('ListArchivedSpawnedThreads', async () => [makeThread('done', { archived: true, spawnedByThreadId: 'agent' })]);
    setBindingMock('ListThreadWakes', async () => ({
      rows: [{ id: 'l1', kind: 'loop', label: 'beat', state: 'armed' }], moreReminders: false,
    }));
    prependThread(agent(true));
    setAgentExpanded('agent', false);
    toggleAgentDone('agent');
    loadArchivedChildren('agent', 1);
    ensureAgentWakes('agent');
    await settle();
    expect(isAgentExpanded('agent')).toBe(false);
    expect(isAgentDoneExpanded('agent')).toBe(true);
    expect(getArchivedChildren('agent', 1)?.length).toBe(1);
    expect(getAgentWakeRows('agent').length).toBe(1);
  }

  function expectCleared(): void {
    expect(isAgentExpanded('agent')).toBe(true);
    expect(isAgentDoneExpanded('agent')).toBe(false);
    expect(getArchivedChildren('agent', 1)).toBeNull();
    expect(getAgentWakeRows('agent')).toEqual([]);
  }

  it('is cleared when the agent is archived (removed from the list)', async () => {
    await primeAgentState();
    removeThread('agent');
    expectCleared();
  });

  it('is cleared when the agent is deleted (a list without it)', async () => {
    await primeAgentState();
    setBindingMock('ListThreads', async () => [makeThread('other')]);
    await refreshThreads();
    expectCleared();
  });

  it("keeps the assistant's fold and footer across a handoff, and drops the old thread's lists", async () => {
    await primeAgentState();
    prependThread(makeThread('agent-next', { agentId: 'agent', archivedChildCount: 2 }));
    replaceThread(makeThread('agent', { pastAgentId: 'agent', archived: true }));
    removeThread('agent');
    expect(isAgentExpanded('agent')).toBe(false);
    expect(isAgentDoneExpanded('agent')).toBe(true);
    expect(getArchivedChildren('agent', 1)).toBeNull();
    expect(getAgentWakeRows('agent')).toEqual([]);
  });

  it('is cleared when the row comes back without the mark', async () => {
    await primeAgentState();
    replaceThread(agent(false));
    expectCleared();
  });
});
