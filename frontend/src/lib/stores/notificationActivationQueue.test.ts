import { describe, expect, it, vi } from 'vitest';
import type { Thread } from '../types/models';
import { createNotificationActivationQueue } from './notificationActivationQueue';

function thread(id: string): Thread {
  return { id } as Thread;
}

function setup() {
  const threads = new Map<string, Thread>();
  const opened: string[] = [];
  const logger = {
    info: vi.fn(),
    warn: vi.fn(),
    error: vi.fn(),
  };
  const showError = vi.fn();
  const queue = createNotificationActivationQueue({
    getThreadById: (id) => threads.get(id),
    loadThreadById: async (id) => threads.get(id) ?? thread(id),
    getWorkflowItem: async (id) => ({ item: { id } }) as never,
    createWorkflowTriageAgent: async (projectId) => thread(`triage:${projectId}`),
    openThread: async (value) => { opened.push(value.id); },
    openWorkflowItem: async (detail) => { opened.push(detail.item.id); },
    openWorkflowsOverview: async () => { opened.push('overview'); },
    showError,
    console: logger,
  });
  return { queue, threads, opened, logger, showError };
}

describe('notification activation queue', () => {
  it('drains a cold-start activation after thread and pane hydration', async () => {
    const { queue, threads, opened, logger } = setup();
    queue.receive({ kind: 'thread', threadId: 'thread-1' });
    expect(queue.pendingCount()).toBe(1);
    expect(opened).toEqual([]);

    threads.set('thread-1', thread('thread-1'));
    await queue.markHydrated();

    expect(opened).toEqual(['thread-1']);
    expect(queue.pendingCount()).toBe(0);
    expect(logger.warn).not.toHaveBeenCalled();
  });

  it('caps pending activations at eight and drops the oldest', async () => {
    const { queue, threads, opened, logger } = setup();
    for (let i = 1; i <= 10; i += 1) {
      const id = `thread-${i}`;
      threads.set(id, thread(id));
      queue.receive({ kind: 'thread', threadId: id });
    }
    expect(queue.pendingCount()).toBe(8);

    await queue.markHydrated();

    expect(opened).toEqual([
      'thread-3',
      'thread-4',
      'thread-5',
      'thread-6',
      'thread-7',
      'thread-8',
      'thread-9',
      'thread-10',
    ]);
    expect(logger.warn).toHaveBeenCalledTimes(2);
    expect(logger.warn).toHaveBeenNthCalledWith(
      1,
      'notification:activated: pending queue full; dropped oldest',
      { kind: 'thread', threadId: 'thread-1' },
    );
  });

  it('preserves FIFO for an activation arriving during the hydration drain', async () => {
    let releaseFirst!: () => void;
    const firstBlocked = new Promise<void>((resolve) => { releaseFirst = resolve; });
    const opened: string[] = [];
    const threads = new Map([
      ['thread-1', thread('thread-1')],
      ['thread-2', thread('thread-2')],
    ]);
    const queue = createNotificationActivationQueue({
      getThreadById: (id) => threads.get(id),
      loadThreadById: async (id) => threads.get(id) ?? thread(id),
      getWorkflowItem: async (id) => ({ item: { id } }) as never,
      createWorkflowTriageAgent: async (projectId) => thread(`triage:${projectId}`),
      openThread: async (value) => {
        opened.push(value.id);
        if (value.id === 'thread-1') await firstBlocked;
      },
      openWorkflowItem: async (detail) => { opened.push(detail.item.id); },
      openWorkflowsOverview: async () => { opened.push('overview'); },
      showError: vi.fn(),
      console: { info: vi.fn(), warn: vi.fn(), error: vi.fn() },
    });
    queue.receive({ kind: 'thread', threadId: 'thread-1' });
    const draining = queue.markHydrated();
    await vi.waitFor(() => expect(opened).toEqual(['thread-1']));

    queue.receive({ kind: 'thread', threadId: 'thread-2' });
    releaseFirst();
    await draining;

    expect(opened).toEqual(['thread-1', 'thread-2']);
  });

  it('routes workflow items and triage agents after hydration', async () => {
    const { queue, opened } = setup();
    queue.receive({ kind: 'workflow-item', workItemId: 'run-1' });
    queue.receive({ kind: 'workflow-triage-agent', projectId: 'project-1' });
    await queue.markHydrated();
    expect(opened).toEqual(['run-1', 'triage:project-1']);
  });

  it('rejects ambiguous and oversized workflow targets', () => {
    const { queue, logger } = setup();
    queue.receive({ kind: 'workflow-item', workItemId: 'run', projectId: 'project' });
    queue.receive({ kind: 'workflow-triage-agent', projectId: 'x'.repeat(257) });
    expect(queue.pendingCount()).toBe(0);
    expect(logger.warn).toHaveBeenCalledTimes(2);
  });

  it('surfaces workflow routing failures without exposing backend details', async () => {
    const { showError, logger } = setup();
    const failure = new Error('private backend detail');
    const failingQueue = createNotificationActivationQueue({
      getThreadById: async () => undefined,
      loadThreadById: async () => { throw failure; },
      getWorkflowItem: async () => { throw failure; },
      createWorkflowTriageAgent: async () => { throw failure; },
      openThread: async () => undefined,
      openWorkflowItem: async () => undefined,
      openWorkflowsOverview: async () => undefined,
      showError,
      console: logger,
    });
    failingQueue.receive({ kind: 'workflow-item', workItemId: 'run' });
    failingQueue.receive({ kind: 'workflow-triage-agent', projectId: 'project' });
    await failingQueue.markHydrated();
    expect(showError.mock.calls).toEqual([
      ['Could not open this workflow run.'],
      ['Could not open the workflow triage agent.'],
    ]);
    expect(logger.error).toHaveBeenCalledTimes(2);
  });
});
