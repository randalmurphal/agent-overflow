import { afterEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { createBackgroundController } from './activityRailBackground.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { noteThread } from '../../transport/entityIndex';
import { __attachBackendForTest, detachBackend } from '../../transport/backends';

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
  return makeItem({ id, status: 'running', isBackground: true, toolName: 'remote_command' });
}
async function flush() { await tick(); await Promise.resolve(); await tick(); }

describe('background tray recovery', () => {
  let release = () => {};
  afterEach(() => { release(); detachBackend(remote); vi.useRealTimers(); });

  it('ignores other computers, fences an old reply, and rehydrates its owner without item events', async () => {
    attachOwner();
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

  it('recovers only relevant owner gaps and removes all recovery listeners on unmount', async () => {
    attachOwner();
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
