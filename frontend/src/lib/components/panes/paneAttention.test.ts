import { describe, expect, it } from 'vitest';
import { resolveThreadStatusPill } from '../../utils/threadStatusPill';
import { setThreadStatus, type ThreadLiveStatus } from '../../stores/threadStatuses.svelte';
import type { Thread } from '../../types/models';
import { resolvePaneAttentionDot } from './paneAttention';

// Prefix avoids collision with the 700+ test files that use plain
// `thread-1`. setThreadStatus and the other status registries are
// module-level and persist across test files within a Vitest worker,
// so a leaked 'thread-1' status would derail the idle assertion below.
function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'pane-attention-test-thread',
    title: 'Thread',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('pane attention helpers', () => {
  it.each<ThreadLiveStatus>([
    'idle',
    'running',
    'awaiting-input',
    'pending-approval',
    'plan-ready',
    'error',
    'interrupted',
  ])('uses the sidebar status resolver classes for %s', (status) => {
    const thread = makeThread(
      status === 'idle'
        ? { lastReadAt: 0, latestTurnCompletedAt: 1 }
        : { id: `pane-attention-test-thread-${status}` },
    );
    if (status !== 'idle') setThreadStatus(thread.id, status);

    const dot = resolvePaneAttentionDot(thread);
    const expected = resolveThreadStatusPill(thread, status);

    expect(dot?.status).toBe(status);
    expect(dot?.pill.dotClass).toBe(expected?.dotClass);
    expect(dot?.pill.pulse).toBe(expected?.pulse);
    expect(dot?.pill.glowClass).toBe(expected?.glowClass);
  });

  it('renders no model for a null-thread pane', () => {
    expect(resolvePaneAttentionDot(null)).toBeNull();
  });
});
