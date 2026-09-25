import { afterEach, describe, expect, it, vi } from 'vitest';
import { createBackgroundController } from './activityRailBackground.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { applyBackgroundTrayEvent } from '../../stores/eventsBackgroundTray';
import type { Item, Thread } from '../../types/models';

describe('tray deltas', () => {
  let release = () => {};
  afterEach(() => {
    release();
    release = () => {};
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  const bash = (id: string, extra: Partial<Item> = {}) => makeItem({
    id, kind: 'tool_call', toolName: 'Bash', status: 'running', isBackground: true, ...extra,
  });

  async function mount(read: () => Promise<Item[]>, thread?: Thread) {
    vi.useFakeTimers();
    const pane = await buildPane(thread);
    const mock = setBindingMock('ListLiveBackgroundTasks', read);
    let controller!: ReturnType<typeof createBackgroundController>;
    release = $effect.root(() => {
      controller = createBackgroundController(() => pane, Date.now);
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(0);
    return { threadId: pane.threadId!, read: mock, controller };
  }

  const rows = (controller: ReturnType<typeof createBackgroundController>) =>
    controller.tasks.map((task) => [task.rowId, task.status]);

  it('adds, replaces and removes the launches a delta answers for, with no read', async () => {
    const { threadId, read, controller } = await mount(async () => [bash('a'), bash('b')]);
    applyBackgroundTrayEvent({ threadId, launchIds: ['c'], rows: [bash('c', { threadId, createdAt: 3 })] });
    applyBackgroundTrayEvent({ threadId, launchIds: ['a'], rows: [] });
    applyBackgroundTrayEvent({
      threadId, launchIds: ['b'], rows: [
        bash('b', { threadId, summary: 'Bash: again' }),
        makeItem({ id: 'b:done', threadId, kind: 'tool_completion', status: 'completed', completionOf: 'b', createdAt: Date.now() }),
      ],
    });
    applyBackgroundTrayEvent({ threadId: 'other-thread', launchIds: ['b'], rows: [] });
    await vi.advanceTimersByTimeAsync(0);
    expect(rows(controller)).toEqual([['b', 'completed'], ['c', 'running']]);
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('applies the deltas that land during a read again over its answer', async () => {
    let finish!: (items: Item[]) => void;
    const { threadId, controller } = await mount(() => new Promise((resolve) => { finish = resolve; }));
    applyBackgroundTrayEvent({ threadId, launchIds: ['a'], rows: [] });
    applyBackgroundTrayEvent({ threadId, launchIds: ['b'], rows: [bash('b', { threadId, summary: 'Bash: newer' })] });
    finish([bash('a'), bash('b', { summary: 'Bash: older' })]);
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.tasks.map((task) => [task.rowId, task.launch?.summary])).toEqual([['b', 'Bash: newer']]);
  });

  it('reads the list for a refresh frame, and for any frame on a Codex thread', async () => {
    const { threadId, read } = await mount(async () => []);
    applyBackgroundTrayEvent({ threadId, refresh: true });
    await vi.advanceTimersByTimeAsync(1_000);
    expect(read).toHaveBeenCalledTimes(2);
    release();
    const codex = await mount(async () => [], makeThread({ provider: 'codex' }));
    applyBackgroundTrayEvent({ threadId: codex.threadId, launchIds: ['x'], rows: [] });
    await vi.advanceTimersByTimeAsync(1_000);
    expect(codex.read).toHaveBeenCalledTimes(2);
  });
});
