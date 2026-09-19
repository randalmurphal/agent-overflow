import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createBackgroundController } from './activityRailBackground.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { noteThread } from '../../transport/entityIndex';
import { __attachBackendForTest, detachBackend } from '../../transport/backends';
import type { Project } from '../../types/models';

const remote = 'tray-owner';
function attachOwner() {
  __attachBackendForTest({ id: remote, name: 'Nexus', backendId: remote, wsUrl: 'wss://nexus.invalid/ws', bootstrapUrl: 'https://nexus.invalid/bootstrap' }, {
    callByID: async () => null, callByName: async () => null, subscribe: () => () => {},
    setWatchedThreads() {}, setPresence() {}, setLease() {}, installStepUpProver() {}, close() {},
    getStatus: () => ({ status: 'connected', nextAttemptAt: null }),
    onStatusChange: (listener: (value: unknown) => void) => { listener({ status: 'connected', nextAttemptAt: null }); return () => {}; },
    getHello: () => null, onHelloChange: () => () => {}, onReplay: () => () => {},
  } as never);
}
function launch(id: string) {
  return makeItem({ id, status: 'running', isBackground: true, toolName: 'MCP/remote_run',
    meta: JSON.stringify({ remoteJob: { computerId: remote, requestId: id } }) });
}
async function flush() { await tick(); await Promise.resolve(); await tick(); }

describe('background tray recovery', () => {
  let release = () => {};
  afterEach(() => { release(); detachBackend(remote); vi.useRealTimers(); });

  it('reads nothing for a draft placeholder, whose synthetic id no computer owns', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    const read = setBindingMock('ListLiveBackgroundTasks', async () => []);
    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      pane.startDraftPlaceholder({ id: 'proj', name: 'proj', path: '/proj', createdAt: 0, updatedAt: 0 } as Project);
      expect(pane.hasDraftPlaceholder).toBe(true);
      release = $effect.root(() => createBackgroundController(() => pane, Date.now).mount());
      await flush();
      emitWailsEvent('provider:background_tasks_changed', { threadId: pane.threadId }, '');
      await flush();
      expect(read).not.toHaveBeenCalled();
      expect(log).not.toHaveBeenCalled();
    } finally { log.mockRestore(); }
  });

  it('ignores other computers, fences an old reply, and rehydrates its owner without item events', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    noteThread(pane.threadId!, remote, 2);
    let finish!: (items: ReturnType<typeof launch>[]) => void;
    const oldRead = setBindingMock('ListLiveBackgroundTasks', () => new Promise((resolve) => { finish = resolve; }));
    let controller!: ReturnType<typeof createBackgroundController>;
    release = $effect.root(() => {
      controller = createBackgroundController(() => pane, Date.now);
      return controller.mount();
    });
    await flush();
    expect(oldRead).toHaveBeenCalledTimes(1);
    __setBackendStatusForTest('', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('', { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(oldRead).toHaveBeenCalledTimes(1);

    __setBackendStatusForTest(remote, { status: 'reconnecting', nextAttemptAt: null });
    const newRead = setBindingMock('ListLiveBackgroundTasks', async () => [launch('current')]);
    __setBackendStatusForTest(remote, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(controller.tasks.map((task) => task.rowId)).toEqual(['current']);
    finish([launch('stale')]);
    await flush();
    expect(controller.tasks.map((task) => task.rowId)).toEqual(['current']);
    expect(newRead).toHaveBeenCalledTimes(1);
  });

  it('keeps running rows through model edits and failed refreshes until a successful removal', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    noteThread(pane.threadId!, remote, 2);
    const read = setBindingMock('ListLiveBackgroundTasks', async () => [launch('running')]);
    let controller!: ReturnType<typeof createBackgroundController>;
    release = $effect.root(() => {
      controller = createBackgroundController(() => pane, Date.now);
      return controller.mount();
    });
    await flush();
    expect(controller.runningCount).toBe(1);
    pane.replaceThread({ ...pane.thread!, model: 'changed-model' });
    await flush();
    expect(controller.tasks.map((task) => task.rowId)).toEqual(['running']);
    expect(read).toHaveBeenCalledTimes(1);

    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    try {
      setBindingMock('ListLiveBackgroundTasks', async () => { throw new Error('read failed'); });
      emitWailsEvent('transport:gap', { channel: 'provider:background_tasks_changed' }, remote);
      await flush();
      expect(log).toHaveBeenCalled();
      expect(controller.tasks.map((task) => task.rowId)).toEqual(['running']);
      setBindingMock('ListLiveBackgroundTasks', async () => []);
      emitWailsEvent('transport:gap', { channel: 'provider:background_tasks_changed' }, remote);
      await flush();
      expect(controller.tasks).toEqual([]);
    } finally { log.mockRestore(); }
  });

  it('recovers only relevant owner gaps and removes all recovery listeners on unmount', async () => {
    attachOwner();
    noteThread('thread-1', remote, 2);
    const pane = await buildPane();
    noteThread(pane.threadId!, remote, 2);
    const read = setBindingMock('ListLiveBackgroundTasks', async () => [launch('running')]);
    release = $effect.root(() => createBackgroundController(() => pane, Date.now).mount());
    await flush();
    expect(read).toHaveBeenCalledTimes(1);
    emitWailsEvent('transport:gap', { channel: 'provider:background_tasks_changed' }, '');
    emitWailsEvent('transport:gap', { channel: 'git:status' }, remote);
    await flush();
    expect(read).toHaveBeenCalledTimes(1);
    emitWailsEvent('transport:gap', { channel: 'provider:background_tasks_changed' }, remote);
    await flush();
    expect(read).toHaveBeenCalledTimes(2);
    release();
    release = () => {};
    emitWailsEvent('transport:gap', { channel: 'provider:background_task_state' }, remote);
    __setBackendStatusForTest(remote, { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest(remote, { status: 'connected', nextAttemptAt: null });
    await flush();
    expect(read).toHaveBeenCalledTimes(2);
  });
});

describe('completion retention', () => {
  let release = () => {};
  afterEach(() => release());

  it('asks for the clock on a nested completion so the pair can prune', async () => {
    const pane = await buildPane();
    const threadId = pane.threadId!;
    const now = Date.now();
    const agent = makeItem({ id: 'agent', threadId, status: 'running', isBackground: true, toolName: 'Agent', createdAt: now - 60_000 });
    const bash = makeItem({ id: 'bash', threadId, status: 'running', isBackground: true, toolName: 'Bash', parentId: 'agent', createdAt: now - 5_000 });
    const done = makeItem({ id: 'bash-done', threadId, status: 'completed', completionOf: 'bash', parentId: 'agent', createdAt: now });
    setBindingMock('ListLiveBackgroundTasks', async () => [agent, bash, done]);
    // The shared clock last ticked before the completion landed; it only
    // advances again once the controller asks for it.
    let clock = $state(now - 30_000);
    let controller!: ReturnType<typeof createBackgroundController>;
    release = $effect.root(() => {
      controller = createBackgroundController(() => pane, () => clock);
      return controller.mount();
    });
    await flush();
    expect(controller.tasks.map((task) => [task.rowId, task.depth, task.status])).toEqual([
      ['agent', 0, 'running'],
      ['bash', 1, 'completed'],
    ]);
    expect(controller.hasPendingCompletion).toBe(true);
    clock = now + 1_000;
    await flush();
    expect(controller.tasks.map((task) => task.rowId)).toEqual(['agent']);
    expect(controller.count).toBe(1);
    expect(controller.hasPendingCompletion).toBe(false);
  });
});
