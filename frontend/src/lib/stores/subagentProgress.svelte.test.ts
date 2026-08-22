import { beforeEach, describe, expect, it } from 'vitest';
import {
  applySubagentProgress,
  clearSubagentProgressForThread,
  dropSubagentProgress,
  liveSubagentProgress,
  resetForTest,
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
