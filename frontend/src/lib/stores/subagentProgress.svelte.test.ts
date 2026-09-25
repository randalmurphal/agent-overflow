import type { Item } from '../types/models';
import { beforeEach, describe, expect, it } from 'vitest';
import {
  applySubagentProgress,
  codexAgentRevision,
  hydrateCodexAgents,
  hydrateSubagentProgress,
  liveCodexAgent,
  clearSubagentProgressForThread,
  dropSubagentProgress,
  liveSubagentProgress,
  resetForTest,
  subagentProgressRevision,
} from './subagentProgress.svelte';

describe('subagentProgress', () => {
  beforeEach(() => {
    resetForTest();
  });

  it('is empty for every launch until a tick lands', () => {
    expect(liveSubagentProgress('t1', 'toolu_1')).toBeUndefined();
    expect(liveSubagentProgress(null, 'toolu_1')).toBeUndefined();
    expect(liveSubagentProgress('t1', '')).toBeUndefined();
  });

  it('stores the latest tick per (thread, launch) and keys threads apart', () => {
    applySubagentProgress({
      threadId: 't1', itemId: 'toolu_1', updatedAt: 10,
      progress: { toolUses: 1, totalTokens: 100, durationMs: 500, activity: 'Reading main.go' },
    });
    applySubagentProgress({
      threadId: 't1', itemId: 'toolu_1', updatedAt: 20,
      progress: { toolUses: 2, totalTokens: 250, durationMs: 900, activity: 'Running tests' },
    });
    expect(liveSubagentProgress('t1', 'toolu_1')).toEqual({
      toolUses: 2, totalTokens: 250, durationMs: 900, activity: 'Running tests', updatedAt: 20,
    });
    expect(liveSubagentProgress('t2', 'toolu_1')).toBeUndefined();
  });

  it('ignores malformed frames', () => {
    applySubagentProgress(undefined);
    applySubagentProgress({ threadId: '', itemId: 'x', updatedAt: 0, progress: {} });
    applySubagentProgress({ threadId: 't1', itemId: '', updatedAt: 0, progress: {} });
    expect(liveSubagentProgress('t1', 'x')).toBeUndefined();
  });

  it('drops one launch when its row settles', () => {
    applySubagentProgress({ threadId: 't1', itemId: 'a', updatedAt: 1, progress: { toolUses: 1 } });
    applySubagentProgress({ threadId: 't1', itemId: 'b', updatedAt: 1, progress: { toolUses: 1 } });
    dropSubagentProgress('t1', 'a');
    expect(liveSubagentProgress('t1', 'a')).toBeUndefined();
    expect(liveSubagentProgress('t1', 'b')).toBeDefined();
  });

  it('drops every launch of a thread on clear and leaves other threads alone', () => {
    applySubagentProgress({ threadId: 't1', itemId: 'a', updatedAt: 1, progress: { toolUses: 1 } });
    applySubagentProgress({ threadId: 't1', itemId: 'b', updatedAt: 1, progress: { toolUses: 1 } });
    applySubagentProgress({ threadId: 't2', itemId: 'a', updatedAt: 1, progress: { toolUses: 1 } });
    clearSubagentProgressForThread('t1');
    expect(liveSubagentProgress('t1', 'a')).toBeUndefined();
    expect(liveSubagentProgress('t1', 'b')).toBeUndefined();
    expect(liveSubagentProgress('t2', 'a')).toBeDefined();
    // A tick after the clear registers the thread again.
    applySubagentProgress({ threadId: 't1', itemId: 'a', updatedAt: 2, progress: { toolUses: 5 } });
    expect(liveSubagentProgress('t1', 'a')?.toolUses).toBe(5);
  });
});

describe('live progress hydration', () => {
  beforeEach(resetForTest);
  const tick = (itemId: string, toolUses: number, threadId = 't1') =>
    ({ threadId, itemId, updatedAt: toolUses, progress: { toolUses } });

  it('installs the snapshot and drops the launches it omits when no tick landed since the read', () => {
    applySubagentProgress(tick('settled', 4));
    const revision = subagentProgressRevision('t1');
    hydrateSubagentProgress('t1', [tick('live', 3)], revision);
    expect(liveSubagentProgress('t1', 'live')).toEqual({ toolUses: 3, updatedAt: 3 });
    expect(liveSubagentProgress('t1', 'settled')).toBeUndefined();
  });

  it('keeps the ticks that landed after the read and fills only what this client lacks', () => {
    const revision = subagentProgressRevision('t1');
    applySubagentProgress(tick('a', 9));
    applySubagentProgress(tick('newer', 2));
    hydrateSubagentProgress('t1', [tick('a', 1), tick('b', 5)], revision);
    expect(liveSubagentProgress('t1', 'a')?.toolUses).toBe(9);
    expect(liveSubagentProgress('t1', 'b')?.toolUses).toBe(5);
    expect(liveSubagentProgress('t1', 'newer')?.toolUses).toBe(2);
  });

  it('does not bring back a launch that settled after the read', () => {
    applySubagentProgress(tick('a', 1));
    const revision = subagentProgressRevision('t1');
    dropSubagentProgress('t1', 'a');
    hydrateSubagentProgress('t1', [tick('a', 1)], revision);
    expect(liveSubagentProgress('t1', 'a')).toBeUndefined();
  });

  it('refuses a snapshot read before the thread was cleared', () => {
    const revision = subagentProgressRevision('t1');
    clearSubagentProgressForThread('t1');
    hydrateSubagentProgress('t1', [tick('a', 1)], revision);
    expect(liveSubagentProgress('t1', 'a')).toBeUndefined();
    hydrateSubagentProgress('t1', [tick('a', 1)], subagentProgressRevision('t1'));
    expect(liveSubagentProgress('t1', 'a')?.toolUses).toBe(1);
  });

  it('keys revisions per thread and ignores another thread\'s entries', () => {
    const revision = subagentProgressRevision('t1');
    applySubagentProgress(tick('x', 1, 't2'));
    hydrateSubagentProgress('t1', [tick('a', 1), tick('b', 1, 't2')], revision);
    expect(liveSubagentProgress('t1', 'a')?.toolUses).toBe(1);
    expect(liveSubagentProgress('t2', 'b')).toBeUndefined();
    expect(liveSubagentProgress('t2', 'x')?.toolUses).toBe(1);
  });
});

describe('Codex runtime hydration', () => {
  beforeEach(resetForTest);
  const agent = (threadId = 't1'): Item => ({
    rev: 0,
    threadId, id: 'spawn', kind: 'tool_call', toolName: 'collab_agent', role: 'assistant',
    turnIndex: 0, itemIndex: 0, status: 'completed', summary: 'Worker', createdAt: 1, updatedAt: 2,
  });
  it('hydrates runtime without erasing progress or decorating history', () => {
    const launch = agent();
    applySubagentProgress({threadId: 't1', itemId: 'spawn', updatedAt: 5, progress: {totalTokens: 42}});
    hydrateCodexAgents('t1', [launch], codexAgentRevision('t1'));
    expect(liveSubagentProgress('t1', 'spawn')?.totalTokens).toBe(42);
    expect(liveCodexAgent('t1', 'spawn')).toEqual(launch);
    expect(launch.meta).toBeUndefined();
  });
  it('rejects snapshots overtaken by a runtime event or teardown', () => {
    const revision = codexAgentRevision('t1');
    applySubagentProgress({threadId: 't1', itemId: 'spawn', updatedAt: 5, progress: {}, codexAgent: agent()});
    hydrateCodexAgents('t1', [], revision);
    expect(liveCodexAgent('t1', 'spawn')).toBeDefined();
    const beforeClear = codexAgentRevision('t1');
    clearSubagentProgressForThread('t1');
    hydrateCodexAgents('t1', [agent()], beforeClear);
    expect(liveCodexAgent('t1', 'spawn')).toBeUndefined();
  });
  it('isolates revisions across threads and clears runtime after progress was dropped', () => {
    const revision = codexAgentRevision('t1');
    hydrateCodexAgents('t2', [agent('t2')], codexAgentRevision('t2'));
    hydrateCodexAgents('t1', [agent()], revision);
    expect(liveCodexAgent('t1', 'spawn')).toBeDefined();
    dropSubagentProgress('t1', 'spawn');
    clearSubagentProgressForThread('t1');
    expect(liveCodexAgent('t1', 'spawn')).toBeUndefined();
    expect(liveCodexAgent('t2', 'spawn')).toBeDefined();
  });
});
