import { flushSync } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  ensureAgentWakes,
  forgetAgentWakes,
  getAgentWakeRows,
  getThreadWakeListing,
  refreshAgentWakes,
  resetThreadWakesForTest,
  retainThreadWakes,
} from './threadWakes.svelte';
import { applyThreadWakesChanged, applyThreadWakesGap } from './eventsThreadWakes';
import { replaceAllThreads } from './threads.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import type { Thread } from '../types/models';
import type { ThreadWakeRow, ThreadWakes } from './bindings';

function row(id: string, overrides: Partial<ThreadWakeRow> = {}): ThreadWakeRow {
  return { id, kind: 'loop', label: id, state: 'armed', ...overrides } as ThreadWakeRow;
}

function thread(id: string, overrides: Partial<Thread> = {}): Thread {
  return {
    id, title: id, provider: 'claude', model: 'test', projectId: 'project-1',
    projectPath: '/tmp/ws', workspacePath: '/tmp/ws', createdAt: 0, updatedAt: 0, archived: false, ...overrides,
  };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 4; i++) await Promise.resolve();
}

describe('thread wakes store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetStagedBackends();
    resetThreadWakesForTest();
    replaceAllThreads([thread('agent', { agentId: 'agent' }), thread('plain')]);
  });

  it('files a row under the computer that answered it, so a later outage there keeps it', async () => {
    stageBackend({ id: 'gpu' });
    let gpuDown = false;
    // The gpu agent is not in the thread list yet: its wake read landed
    // before the catalog did.
    setBindingMock('ListStandingAgentWakes', async () => {
      if (takePinnedBackend() !== 'gpu') return [{ threadId: 'agent', rows: [row('l1')] }];
      if (gpuDown) throw new Error('offline');
      return [{ threadId: 'gpu-agent', rows: [row('g1')] }];
    });
    await refreshAgentWakes();
    expect(getAgentWakeRows('gpu-agent').map((r) => r.id)).toEqual(['g1']);
    gpuDown = true;
    await refreshAgentWakes();
    expect(getAgentWakeRows('gpu-agent').map((r) => r.id)).toEqual(['g1']);
    expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['l1']);
  });

  it('loads every agent\'s live loops and watches, dropping reminders', async () => {
    setBindingMock('ListStandingAgentWakes', async () => [
      { threadId: 'agent', rows: [row('l1'), row('r1', { kind: 'reminder' }), row('w1', { kind: 'watch' })] },
    ]);
    await refreshAgentWakes();
    expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['l1', 'w1']);
    expect(getAgentWakeRows('plain')).toEqual([]);
  });

  it('reads one row on demand, once, and leaves a known row alone', async () => {
    const list = setBindingMock('ListThreadWakes', async () => ({ rows: [row('l1')], moreReminders: false }));
    ensureAgentWakes('agent');
    ensureAgentWakes('agent');
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['l1']);
    ensureAgentWakes('agent');
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
  });

  it('remembers a thread read with no rows, and reads it again only once forgotten', async () => {
    const list = setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: false }));
    ensureAgentWakes('agent');
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(getAgentWakeRows('agent')).toEqual([]);
    ensureAgentWakes('agent');
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    // Demoted and promoted again: the rows are read afresh.
    forgetAgentWakes('agent');
    ensureAgentWakes('agent');
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('discards a read that lands after the thread was forgotten', async () => {
    let answer: (wakes: { rows: ThreadWakeRow[]; moreReminders: boolean }) => void = () => {};
    setBindingMock('ListThreadWakes', () => new Promise((resolve) => { answer = resolve; }));
    ensureAgentWakes('agent');
    await flush();
    forgetAgentWakes('agent');
    answer({ rows: [row('l1')], moreReminders: false });
    await flush();
    expect(getAgentWakeRows('agent')).toEqual([]);
  });

  it('applies a changed row, drops a removed one, and re-reads an unreadable one', async () => {
    setBindingMock('ListStandingAgentWakes', async () => [{ threadId: 'agent', rows: [row('l1')] }]);
    await refreshAgentWakes();
    applyThreadWakesChanged({ threadId: 'agent', id: 'l1', row: row('l1', { state: 'running' }) }, HOME_BACKEND);
    expect(getAgentWakeRows('agent')[0]?.state).toBe('running');
    applyThreadWakesChanged({ threadId: 'agent', id: 'w1', row: row('w1', { kind: 'watch' }) }, HOME_BACKEND);
    expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['l1', 'w1']);
    applyThreadWakesChanged({ threadId: 'agent', id: 'l1', removed: true }, HOME_BACKEND);
    expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['w1']);

    const list = setBindingMock('ListThreadWakes', async () => ({ rows: [row('w2', { kind: 'watch' })], moreReminders: false }));
    applyThreadWakesChanged({ threadId: 'agent', id: 'w1' }, HOME_BACKEND);
    await flush();
    expect(list).toHaveBeenCalledWith('agent');
    expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['w2']);
  });

  it('ignores reminders and rows of threads that are not standing agents', () => {
    applyThreadWakesChanged({ threadId: 'agent', id: 'r1', row: row('r1', { kind: 'reminder' }) }, HOME_BACKEND);
    applyThreadWakesChanged({ threadId: 'plain', id: 'l1', row: row('l1') }, HOME_BACKEND);
    expect(getAgentWakeRows('agent')).toEqual([]);
    expect(getAgentWakeRows('plain')).toEqual([]);
    // A thread promoted since boot gets its first row from the frame.
    applyThreadWakesChanged({ threadId: 'agent', id: 'l1', row: row('l1') }, HOME_BACKEND);
    expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['l1']);
  });

  it('re-reads every computer on a gap', async () => {
    const list = setBindingMock('ListStandingAgentWakes', async () => [{ threadId: 'agent', rows: [row('l9')] }]);
    applyThreadWakesGap(HOME_BACKEND);
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
    expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['l9']);
  });

  it('drops the row when the last loop is cancelled, and the reset clears everything', async () => {
    setBindingMock('ListStandingAgentWakes', async () => [{ threadId: 'agent', rows: [row('l1')] }]);
    await refreshAgentWakes();
    applyThreadWakesChanged({ threadId: 'agent', id: 'l1', removed: true }, HOME_BACKEND);
    expect(getAgentWakeRows('agent')).toEqual([]);
    expect(getAgentWakeRows('agent')).toBe(getAgentWakeRows('plain'));
  });

  it('reruns only the rows of the thread a frame names', async () => {
    const busy = Array.from({ length: 200 }, (_, i) => `busy-${i}`);
    const idle = Array.from({ length: 100 }, (_, i) => `idle-${i}`);
    replaceAllThreads([...busy, ...idle].map((id) => thread(id, { agentId: id })));
    setBindingMock('ListStandingAgentWakes', async () => busy.map((threadId) => ({ threadId, rows: [row(`l-${threadId}`)] })));
    setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: 0 }));
    await refreshAgentWakes();
    // Idle agents are absent from the bulk read; each row reads its own.
    for (const id of idle) ensureAgentWakes(id);
    await flush();

    // One effect per mounted agent row, as AgentStateLine derives its line.
    const runs = new Map<string, number>();
    const unmount = $effect.root(() => {
      for (const id of [...busy, ...idle]) {
        $effect(() => {
          runs.set(id, (runs.get(id) ?? 0) + 1);
          getAgentWakeRows(id);
        });
      }
    });
    flushSync();
    const reran = (frame: () => void) => {
      const before = new Map(runs);
      frame();
      flushSync();
      return [...runs].filter(([id, n]) => n !== before.get(id)).map(([id]) => id);
    };

    // A changed row, an idle agent's first loop, and a busy agent's last.
    expect(reran(() => applyThreadWakesChanged({ threadId: 'busy-7', id: 'l-busy-7', row: row('l-busy-7', { state: 'running' }) }, HOME_BACKEND))).toEqual(['busy-7']);
    expect(reran(() => applyThreadWakesChanged({ threadId: 'idle-3', id: 'l1', row: row('l1') }, HOME_BACKEND))).toEqual(['idle-3']);
    expect(reran(() => applyThreadWakesChanged({ threadId: 'busy-8', id: 'l-busy-8', removed: true }, HOME_BACKEND))).toEqual(['busy-8']);
    // A reminder is not the sidebar's: no row reruns.
    expect(reran(() => applyThreadWakesChanged({ threadId: 'busy-9', id: 'r1', row: row('r1', { kind: 'reminder' }) }, HOME_BACKEND))).toEqual([]);
    // A resync that finds what the store holds reruns nothing.
    setBindingMock('ListStandingAgentWakes', async () => [
      ...busy.filter((id) => id !== 'busy-8').map((threadId) => ({ threadId, rows: [row(`l-${threadId}`, threadId === 'busy-7' ? { state: 'running' } : {})] })),
      { threadId: 'idle-3', rows: [row('l1')] },
    ]);
    const before = new Map(runs);
    await refreshAgentWakes();
    flushSync();
    expect([...runs].filter(([id, n]) => n !== before.get(id))).toEqual([]);
    unmount();
  });

  describe('a retained listing', () => {
    afterEach(() => {
      vi.useRealTimers();
    });

    function reminder(id: string, nextAt: number): ThreadWakeRow {
      return row(id, { kind: 'reminder', nextAt });
    }

    async function retain(threadId: string, wakes: ThreadWakes) {
      vi.useFakeTimers();
      const read = setBindingMock('ListThreadWakes', async () => wakes);
      const release = retainThreadWakes(threadId);
      await vi.advanceTimersByTimeAsync(0);
      return { read, release };
    }

    it('settles a reminder at the bound from the frame while it holds every reminder', async () => {
      const listed = Array.from({ length: 64 }, (_, i) => reminder(`r${String(i).padStart(2, '0')}`, 1000 + i));
      const { read, release } = await retain('plain', { rows: listed, moreReminders: 0 });
      applyThreadWakesChanged({ threadId: 'plain', id: 'early', row: reminder('early', 1) }, HOME_BACKEND);
      await vi.advanceTimersByTimeAsync(500);
      expect(read).toHaveBeenCalledTimes(1);
      const listing = getThreadWakeListing('plain')!;
      expect(listing.reminders.map((r) => r.id)).toEqual(['early', ...listed.slice(0, 63).map((r) => r.id)]);
      expect(listing.moreReminders).toBe(1);

      // One unlisted reminder exists now: removing a listed one brings it in,
      // which only the backend knows.
      applyThreadWakesChanged({ threadId: 'plain', id: 'early', removed: true }, HOME_BACKEND);
      await vi.advanceTimersByTimeAsync(500);
      expect(read).toHaveBeenCalledTimes(2);
      release();
    });

    it('applies a frame that lands while its read is out over the answer', async () => {
      vi.useFakeTimers();
      let answer: (wakes: ThreadWakes) => void = () => {};
      setBindingMock('ListThreadWakes', () => new Promise((resolve) => { answer = resolve; }));
      const release = retainThreadWakes('plain');
      await vi.advanceTimersByTimeAsync(0);
      applyThreadWakesChanged({ threadId: 'plain', id: 'l1', row: row('l1', { state: 'running' }) }, HOME_BACKEND);
      applyThreadWakesChanged({ threadId: 'plain', id: 'r1', removed: true }, HOME_BACKEND);
      // The answer was read before both writes.
      answer({ rows: [row('l1'), reminder('r1', 5)], moreReminders: 0 });
      await vi.advanceTimersByTimeAsync(0);
      const listing = getThreadWakeListing('plain')!;
      expect(listing.loops.map((r) => [r.id, r.state])).toEqual([['l1', 'running']]);
      expect(listing.reminders).toEqual([]);
      release();
    });

    it('lets go of a thread the sidebar does not show, and of an agent\'s reminders alone', async () => {
      const plain = await retain('plain', { rows: [row('l1'), reminder('r1', 5)], moreReminders: 0 });
      expect(getAgentWakeRows('plain').map((r) => r.id)).toEqual(['l1']);
      plain.release();
      plain.release();
      expect(getThreadWakeListing('plain')).toBeNull();
      expect(getAgentWakeRows('plain')).toEqual([]);

      ensureAgentWakes('agent');
      const agent = await retain('agent', { rows: [row('l2'), reminder('r2', 5)], moreReminders: 0 });
      agent.release();
      expect(getThreadWakeListing('agent')).toBeNull();
      expect(getAgentWakeRows('agent').map((r) => r.id)).toEqual(['l2']);
      // A frame for a thread no surface shows is not kept.
      applyThreadWakesChanged({ threadId: 'plain', id: 'l3', row: row('l3') }, HOME_BACKEND);
      expect(getAgentWakeRows('plain')).toEqual([]);
    });
  });
});
