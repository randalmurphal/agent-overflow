import { describe, expect, it, vi } from 'vitest';
import type { Thread } from '../types/models';
import { createNotificationActivationQueue } from './notificationActivationQueue';

function thread(id: string): Thread {
  return { id } as Thread;
}

function setup() {
  const threads = new Map<string, Thread>();
  const opened: string[] = [];
  const runs: string[] = [];
  const logger = {
    info: vi.fn(),
    warn: vi.fn(),
    error: vi.fn(),
  };
  const queue = createNotificationActivationQueue({
    getThreadById: (id) => threads.get(id),
    openThread: async (value) => { opened.push(value.id); },
    openWorkflowRun: async (workItemId) => { runs.push(workItemId); },
    console: logger,
  });
  return { queue, threads, opened, runs, logger };
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
      openThread: async (value) => {
        opened.push(value.id);
        if (value.id === 'thread-1') await firstBlocked;
      },
      openWorkflowRun: async () => undefined,
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

  it('serializes activations received after hydration', async () => {
    let releaseFirst!: () => void;
    const firstBlocked = new Promise<void>((resolve) => { releaseFirst = resolve; });
    const opened: string[] = [];
    const threads = new Map([
      ['thread-1', thread('thread-1')],
      ['thread-2', thread('thread-2')],
    ]);
    const queue = createNotificationActivationQueue({
      getThreadById: (id) => threads.get(id),
      openThread: async (value) => {
        opened.push(value.id);
        if (value.id === 'thread-1') await firstBlocked;
      },
      openWorkflowRun: async () => undefined,
      console: { info: vi.fn(), warn: vi.fn(), error: vi.fn() },
    });
    await queue.markHydrated();

    queue.receive({ kind: 'thread', threadId: 'thread-1' });
    queue.receive({ kind: 'thread', threadId: 'thread-2' });
    await vi.waitFor(() => expect(opened).toEqual(['thread-1']));
    releaseFirst();
    await vi.waitFor(() => expect(opened).toEqual(['thread-1', 'thread-2']));
  });

  it('rejects unsupported, ambiguous, and oversized targets', () => {
    const { queue, logger } = setup();
    queue.receive({ kind: 'thread', threadId: 'thread-1', projectId: 'project' });
    queue.receive({ kind: 'thread', threadId: 'x'.repeat(257) });
    queue.receive({ kind: 'workflow-item', workItemId: 'x'.repeat(257) });
    queue.receive({ kind: 'workflow-item', workItemId: 'run-1', threadId: 'thread-1' });
    queue.receive({ kind: 'workflow-item' });
    queue.receive({ kind: 'none', threadId: 'thread-1' });
    expect(queue.pendingCount()).toBe(0);
    expect(logger.warn).toHaveBeenCalledTimes(6);
  });

  it('routes a workflow-item deep link to the overlay, cold start included', async () => {
    const { queue, runs } = setup();
    queue.receive({ kind: 'workflow-item', workItemId: 'run-1' });
    expect(queue.pendingCount()).toBe(1);
    expect(runs).toEqual([]);

    await queue.markHydrated();
    expect(runs).toEqual(['run-1']);

    queue.receive({ kind: 'workflow-item', workItemId: 'run-2' });
    await vi.waitFor(() => expect(runs).toEqual(['run-1', 'run-2']));
  });

  it('logs a workflow deep-link failure without dropping the queue', async () => {
    const logger = { info: vi.fn(), warn: vi.fn(), error: vi.fn() };
    const failingQueue = createNotificationActivationQueue({
      getThreadById: () => undefined,
      openThread: async () => undefined,
      openWorkflowRun: async () => { throw new Error('private backend detail'); },
      console: logger,
    });
    failingQueue.receive({ kind: 'workflow-item', workItemId: 'run-1' });
    await failingQueue.markHydrated();
    expect(logger.error).toHaveBeenCalledTimes(1);
    expect(failingQueue.pendingCount()).toBe(0);
  });

  it('logs an activation failure without dropping the queue', async () => {
    const logger = { info: vi.fn(), warn: vi.fn(), error: vi.fn() };
    const failingQueue = createNotificationActivationQueue({
      getThreadById: async () => { throw new Error('private backend detail'); },
      openThread: async () => undefined,
      openWorkflowRun: async () => undefined,
      console: logger,
    });
    failingQueue.receive({ kind: 'thread', threadId: 'thread-1' });
    await failingQueue.markHydrated();
    expect(logger.error).toHaveBeenCalledTimes(1);
    expect(failingQueue.pendingCount()).toBe(0);
  });
});
