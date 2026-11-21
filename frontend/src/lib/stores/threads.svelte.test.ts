import { describe, expect, it, vi } from 'vitest';
import {
  getThreads,
  getThreadLiveActivityAt,
  replaceAllThreads,
  prependThread,
  prependThreadReply,
  removeThread,
  updateThreadTitle,
  updateThreadModel,
  replaceThread,
  touchThreadActivity,
  clearThreadGroupMembership,
  dropThreadsForDetachedBackend,
} from './threads.svelte';
import { loadSidebarCatalogs } from './sidebarCatalogs';
import type { Thread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import {
  ensureAgentWakes,
  getAgentWakeRows,
  resetThreadWakesForTest,
} from './threadWakes.svelte';
import * as replicaSession from '../replica/session';
import { observeCatalogStamp, resetCatalogStampsForTest } from '../replica/catalogStamp';
import { HOME_BACKEND } from '../transport/backendKey';
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
  // A group delete leaves its members standalone on the backend; the
  // client's rows say the same, so a spawned member keeps its row as a
  // plain one.
  it('leaves the members of a deleted group standalone', async () => {
    setBindingMock('ListThreads', async () => [
      makeThread('member', { groupId: 'g1', placement: 'grouped', pinnedAt: 5 }),
      makeThread('other', { groupId: 'g2', placement: 'grouped' }),
    ]);
    await loadSidebarCatalogs();
    clearThreadGroupMembership('g1');
    expect(getThreads().map((t) => [t.id, t.groupId, t.placement, t.pinnedAt])).toEqual([
      ['member', undefined, 'standalone', undefined],
      ['other', 'g2', 'grouped', undefined],
    ]);
  });

  describe('loadSidebarCatalogs()', () => {
    it('replaces the store with the RPC result', async () => {
      const loaded = [makeThread('t1'), makeThread('t2')];
      setBindingMock('ListThreads', async () => loaded);
      await loadSidebarCatalogs();
      expect(getThreads().map((t) => t.id)).toEqual(['t1', 't2']);
    });

    it('leaves the store alone on failure', async () => {
      // Seed known state.
      setBindingMock('ListThreads', async () => [makeThread('keep')]);
      await loadSidebarCatalogs();
      expect(getThreads().map((t) => t.id)).toEqual(['keep']);

      // Failing RPC.
      setBindingMock('ListThreads', async () => { throw new Error('rpc down'); });
      const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
      await loadSidebarCatalogs();
      // Prior value preserved.
      expect(getThreads().map((t) => t.id)).toEqual(['keep']);
      consoleErr.mockRestore();
    });
  });

  describe('rows as values', () => {
    it('rejects a write into a stored row or into the list', async () => {
      setBindingMock('ListThreads', async () => [makeThread('t1'), makeThread('t2')]);
      await loadSidebarCatalogs();
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
      await loadSidebarCatalogs();
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

    it('writes its catalog with a row cache, since its rows are values', async () => {
      const write = vi.spyOn(replicaSession, 'putReplicaCatalog').mockResolvedValue(undefined);
      observeCatalogStamp(HOME_BACKEND, 'threads', 'stamp');
      setBindingMock('ListThreads', async () => [makeThread('t1')]);
      await loadSidebarCatalogs();
      // The read writes its answer as it came; the store's writer follows
      // with the rows it holds, and that is the write with the cache.
      const cached = () => write.mock.calls.filter((call) => call[1] === 'threads' && call[5] instanceof WeakMap);
      // An earlier test's write may still hold the spacing.
      await vi.waitFor(() => expect(cached().length).toBeGreaterThan(0), { timeout: 4000 });
      expect(cached()[0][2]).toEqual([makeThread('t1')]);
      write.mockRestore();
      resetCatalogStampsForTest();
    });

    it('keeps its own list when a caller hands over an array', async () => {
      setBindingMock('ListThreads', async () => [makeThread('t1'), makeThread('t2')]);
      await loadSidebarCatalogs();
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
      await loadSidebarCatalogs();
      prependThread(makeThread('a'));
      expect(getThreads().map((t) => t.id)).toEqual(['a', 'b', 'c']);
    });

    // A create's reply is read before its `listed` echo leaves, and a
    // later write's frame can land before the reply too.
    it('prependThreadReply keeps a frame that arrived during the call', async () => {
      setBindingMock('ListThreads', async () => [makeThread('b')]);
      await loadSidebarCatalogs();
      const mark = threadRowFrameMark();
      applyThreadUpdated({ action: 'listed', thread: makeThread('fresh', { title: 'Named meanwhile' }) });
      const held = prependThreadReply(makeThread('fresh'), mark);
      expect(held.title).toBe('Named meanwhile');
      expect(getThreads().map((t) => [t.id, t.title])).toEqual([['fresh', 'Named meanwhile'], ['b', 'Thread b']]);
      const later = threadRowFrameMark();
      expect(prependThreadReply(makeThread('fresh', { title: 'Reply' }), later).title).toBe('Reply');
    });

    it('prependThreadReply applies a ready fork answer over the preparing row a frame set', async () => {
      setBindingMock('ListThreads', async () => [makeThread('b')]);
      await loadSidebarCatalogs();
      const mark = threadRowFrameMark();
      applyThreadUpdated({ action: 'listed', thread: makeThread('fork', { forkPreparing: true }) });
      const applied = prependThreadReply(makeThread('fork', { sessionRef: 'ready-session' }), mark);
      expect(applied.forkPreparing ?? false).toBe(false);
      expect(applied.sessionRef).toBe('ready-session');
      expect(getThreads().map((t) => [t.id, t.forkPreparing ?? false])).toEqual([['fork', false], ['b', false]]);
    });

    it('removeThread drops the matching id and leaves others', async () => {
      setBindingMock('ListThreads', async () => [
        makeThread('a'),
        makeThread('b'),
        makeThread('c'),
      ]);
      await loadSidebarCatalogs();
      removeThread('b');
      expect(getThreads().map((t) => t.id)).toEqual(['a', 'c']);
    });

    it('removeThread on missing id is a no-op', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a')]);
      await loadSidebarCatalogs();
      removeThread('missing');
      expect(getThreads().map((t) => t.id)).toEqual(['a']);
    });

    it('updateThreadTitle replaces only the matching thread title', async () => {
      setBindingMock('ListThreads', async () => [
        makeThread('a', { title: 'A' }),
        makeThread('b', { title: 'B' }),
      ]);
      await loadSidebarCatalogs();
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
      await loadSidebarCatalogs();
      updateThreadModel('b', 'opus');

      expect(getThreads().find((t) => t.id === 'a')?.model).toBe('sonnet');
      expect(getThreads().find((t) => t.id === 'b')?.model).toBe('opus');
    });

    it('replaceThread swaps the thread object wholesale', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a', { title: 'old' })]);
      await loadSidebarCatalogs();
      replaceThread(makeThread('a', { title: 'new', model: 'gpt-5' }));

      const found = getThreads().find((t) => t.id === 'a');
      expect(found?.title).toBe('new');
      expect(found?.model).toBe('gpt-5');
    });

    it('replaceThread is a no-op when id does not exist', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a')]);
      await loadSidebarCatalogs();
      replaceThread(makeThread('missing', { title: 'x' }));
      expect(getThreads()).toHaveLength(1);
      expect(getThreads()[0].id).toBe('a');
    });

    it('touchThreadActivity bumps the live box and leaves the array signal silent', async () => {
      setBindingMock('ListThreads', async () => [makeThread('a', { updatedAt: 100 })]);
      await loadSidebarCatalogs();
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
      await loadSidebarCatalogs();

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
      await loadSidebarCatalogs();
      touchThreadActivity('a', 900);
      removeThread('a');

      prependThread(makeThread('a', { updatedAt: 200 }));
      expect(getThreadLiveActivityAt(getThreads()[0])).toBe(200);
    });
  });
});

// A thread that stops being an assistant's current thread in the list takes
// its own state with it, whichever way it left.
describe('standing agent state on removal', () => {
  const agent = (promoted: boolean) => makeThread('agent', promoted ? { agentId: 'agent', placement: 'standalone' } : {});

  async function settle(): Promise<void> {
    for (let i = 0; i < 6; i += 1) await Promise.resolve();
  }

  async function primeAgentState(): Promise<void> {
    resetThreadWakesForTest();
    setBindingMock('ListThreadWakes', async () => ({
      rows: [{ id: 'l1', kind: 'loop', label: 'beat', state: 'armed' }], moreReminders: false,
    }));
    prependThread(agent(true));
    ensureAgentWakes('agent');
    await settle();
    expect(getAgentWakeRows('agent').length).toBe(1);
  }

  function expectCleared(): void {
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
    await loadSidebarCatalogs();
    expectCleared();
  });

  it("drops the old thread's state across a handoff", async () => {
    await primeAgentState();
    prependThread(makeThread('agent-next', { agentId: 'agent' }));
    replaceThread(makeThread('agent', { pastAgentId: 'agent', archived: true }));
    removeThread('agent');
    expectCleared();
  });

  it('holds one current thread per assistant while a handoff lists the new thread before the old one leaves', async () => {
    await primeAgentState();
    prependThread(makeThread('agent-next', { agentId: 'agent', createdAt: 5 }));
    const current = getThreads().filter((t) => t.agentId === 'agent');
    expect(current.map((t) => t.id)).toEqual(['agent-next']);
    // The old thread keeps its placement, as the handoff does.
    expect(getThreads().find((t) => t.id === 'agent')).toMatchObject({ agentId: undefined, pastAgentId: 'agent', placement: 'standalone' });
    // Two new rows of one assistant in one write: the newest is current.
    setBindingMock('ListThreads', async () => [
      makeThread('a', { agentId: 'other', createdAt: 1 }),
      makeThread('b', { agentId: 'other', createdAt: 2 }),
    ]);
    await loadSidebarCatalogs();
    expect(getThreads().filter((t) => t.agentId === 'other').map((t) => t.id)).toEqual(['b']);
  });

  it('is cleared when the row comes back without the mark', async () => {
    await primeAgentState();
    replaceThread(agent(false));
    expectCleared();
  });

  it("keeps the assistant's state when its computer detaches", async () => {
    await primeAgentState();
    dropThreadsForDetachedBackend(['agent']);
    expect(getThreads().some((t) => t.id === 'agent')).toBe(false);
    expect(getAgentWakeRows('agent').length).toBe(1);
  });
});
