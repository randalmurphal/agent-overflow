import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import { formatToolUses, persistedSubagentProgress, resolveSubagentProgress } from './subagentProgress';

function item(overrides: Partial<Item>): Item {
  return {
    id: 'toolu_1',
    threadId: 't1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'running',
    summary: 'Agent: review',
    toolName: 'Agent',
    createdAt: 1,
    updatedAt: 1,
    ...overrides,
  };
}

describe('resolveSubagentProgress', () => {
  it('is empty with no source', () => {
    expect(resolveSubagentProgress(item({}), null, undefined)).toEqual({
      toolUses: null, totalTokens: null, durationMs: null, activity: '', source: 'none',
    });
  });

  it('uses the live tick while running, including the activity line', () => {
    const got = resolveSubagentProgress(item({}), null, {
      toolUses: 3, totalTokens: 18_227, durationMs: 2_368, activity: ' Running Sleep ',
    });
    expect(got).toEqual({
      toolUses: 3, totalTokens: 18_227, durationMs: 2_368, activity: 'Running Sleep', source: 'live',
    });
  });

  it('falls back to persisted numbers while running when no tick has landed', () => {
    const got = resolveSubagentProgress(
      item({ meta: JSON.stringify({ subagentProgress: { toolUses: 2, totalTokens: 900 } }) }),
      null,
      undefined,
    );
    expect(got.source).toBe('persisted');
    expect(got.toolUses).toBe(2);
    expect(got.activity).toBe('');
  });

  it('prefers the persisted final numbers over a stale live tick once settled', () => {
    const got = resolveSubagentProgress(
      item({ status: 'completed', meta: JSON.stringify({ subagentProgress: { toolUses: 7, totalTokens: 4_000, durationMs: 12_000 } }) }),
      null,
      { toolUses: 4, totalTokens: 1_000, activity: 'Running tests' },
    );
    expect(got).toEqual({
      toolUses: 7, totalTokens: 4_000, durationMs: 12_000, activity: '', source: 'persisted',
    });
  });

  it('keeps a live tick (without activity) on a settled row that persisted nothing', () => {
    const got = resolveSubagentProgress(item({ status: 'completed' }), null, { toolUses: 4, activity: 'x' });
    expect(got.toolUses).toBe(4);
    expect(got.activity).toBe('');
  });

  it('drops zero and malformed counters', () => {
    const got = resolveSubagentProgress(item({}), null, {
      toolUses: 0, totalTokens: Number.NaN, durationMs: -5,
    });
    expect(got.toolUses).toBeNull();
    expect(got.totalTokens).toBeNull();
    expect(got.durationMs).toBeNull();
  });

  it('reads a detached launch off its completion record and ignores the launch row', () => {
    // docs/specs/agent-visibility.md §Immutable agent history: the spawn
    // row never carries settled values; the completion record does.
    const launch = item({
      isBackground: true,
      meta: JSON.stringify({ subagentProgress: { toolUses: 1, totalTokens: 10 } }),
    });
    const completion = item({
      id: 'complete:toolu_1', kind: 'tool_completion', completionOf: 'toolu_1', status: 'completed',
      meta: JSON.stringify({ subagentProgress: { toolUses: 9, totalTokens: 61_000, durationMs: 365_000 } }),
    });
    expect(resolveSubagentProgress(launch, completion, undefined)).toEqual({
      toolUses: 9, totalTokens: 61_000, durationMs: 365_000, activity: '', source: 'persisted',
    });
    // Liveness comes from the completion record by default.
    const running = { ...completion, status: 'running' as const };
    expect(resolveSubagentProgress(launch, running, { toolUses: 2, activity: 'Reading' }).source).toBe('live');
    // A completion without numbers is the answer; the launch row is not a fallback.
    const bare = { ...completion, meta: '' };
    expect(resolveSubagentProgress(launch, bare, undefined).source).toBe('none');
  });

  it('reads persisted progress defensively', () => {
    expect(persistedSubagentProgress(item({ meta: 'garbage' }))).toBeNull();
    expect(persistedSubagentProgress(item({ meta: JSON.stringify({ subagentProgress: [1] }) }))).toBeNull();
  });
});

describe('formatToolUses', () => {
  it('pluralises', () => {
    expect(formatToolUses(null)).toBe('');
    expect(formatToolUses(1)).toBe('1 tool');
    expect(formatToolUses(12)).toBe('12 tools');
  });
});
