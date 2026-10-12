import { beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  ensureAgentWakes,
  getAgentWakeRows,
  refreshAgentWakes,
  resetAgentWakesForTest,
} from './agentWakes.svelte';
import { applyThreadWakesChanged, applyThreadWakesGap } from './eventsThreadWakes';
import { replaceAllThreads } from './threads.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import type { Thread } from '../types/models';
import type { ThreadWakeRow } from './bindings';

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

describe('agentWakes store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetStagedBackends();
    resetAgentWakesForTest();
    replaceAllThreads([thread('agent', { agentPromotedAt: 1 }), thread('plain')]);
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
});
