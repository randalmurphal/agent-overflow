import { afterEach, describe, expect, it, vi } from 'vitest';
import { createWakeController, type WakeController } from './activityRailWakes.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { __setBackendStatusForTest } from '../../stores/transportStatus.svelte';
import { noteThread } from '../../transport/entityIndex';
import { setupEventListeners } from '../../stores/events';
import { __attachBackendForTest, detachBackend } from '../../transport/backends';
import type { Project } from '../../types/models';
import type { ThreadWakeRow } from '../../stores/bindings';

// A second computer, so a frame's origin resolves to it.
const remote = 'wake-owner';
function attachRemote() {
  __attachBackendForTest({ id: remote, name: 'Nexus', backendId: remote, wsUrl: 'wss://nexus.invalid/ws', bootstrapUrl: 'https://nexus.invalid/bootstrap' }, {
    callByID: async () => null, callByName: async () => null, subscribe: () => () => {},
    setWatchedThreads() {}, setPresence() {}, setLease() {}, installStepUpProver() {}, close() {},
    getStatus: () => ({ status: 'connected', nextAttemptAt: null }),
    onStatusChange: (listener: (value: unknown) => void) => { listener({ status: 'connected', nextAttemptAt: null }); return () => {}; },
    getHello: () => null, onHelloChange: () => () => {}, onReplay: () => () => {},
  } as never);
}

function row(id: string, over: Partial<ThreadWakeRow> = {}): ThreadWakeRow {
  return { id, kind: 'loop', label: id, state: 'armed', nextAt: Date.now() + 60_000, ...over } as ThreadWakeRow;
}

describe('wake controller', () => {
  let release = () => {};
  let stopEvents = () => {};
  afterEach(() => {
    release();
    release = () => {};
    stopEvents();
    stopEvents = () => {};
    detachBackend(remote);
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  async function mountWakes(rows: ThreadWakeRow[], moreReminders = 0) {
    vi.useFakeTimers();
    attachRemote();
    noteThread('thread-1', '', 2);
    const pane = await buildPane();
    noteThread(pane.threadId!, '', 2);
    const read = setBindingMock('ListThreadWakes', async () => ({ rows, moreReminders }));
    // The frames reach the controller through the app's event routing.
    stopEvents = setupEventListeners();
    let controller!: WakeController;
    release = $effect.root(() => {
      controller = createWakeController(() => pane);
      return controller.mount();
    });
    await vi.advanceTimersByTimeAsync(0);
    return { pane, read, controller };
  }

  it('reads its thread on open and counts listed and unlisted rows, pulsing only for work in flight', async () => {
    const { pane, read, controller } = await mountWakes([
      row('tests', { state: 'running' }),
      row('logs', { kind: 'watch', state: 'running', longRunning: true }),
      row('token-1', { kind: 'reminder' }),
    ], 5);
    expect(read).toHaveBeenCalledWith(pane.threadId);
    expect(controller.rows.map((r) => r.id)).toEqual(['tests', 'logs', 'token-1']);
    expect(controller.moreReminders).toBe(5);
    expect(controller.count).toBe(8);
    expect(controller.runningCount).toBe(1);
  });

  it('reads again for a change to its thread, not another, and for a lost frame on its computer', async () => {
    const { pane, read, controller } = await mountWakes([row('tests')]);
    expect(read).toHaveBeenCalledTimes(1);
    emitWailsEvent('thread:wakes_changed', { threadId: 'other-thread', id: 'wake-1' });
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(1);

    setBindingMock('ListThreadWakes', async () => ({ rows: [row('tests', { lastOutcome: 'woke the thread' })], moreReminders: 0 }));
    emitWailsEvent('thread:wakes_changed', { threadId: pane.threadId, id: 'wake-1' });
    await vi.advanceTimersByTimeAsync(500);
    expect(controller.rows[0].lastOutcome).toBe('woke the thread');

    const gapRead = setBindingMock('ListThreadWakes', async () => ({ rows: [], moreReminders: 0 }));
    emitWailsEvent('transport:gap', { channel: 'thread:wakes_changed' }, remote);
    await vi.advanceTimersByTimeAsync(500);
    expect(gapRead).not.toHaveBeenCalled();
    emitWailsEvent('transport:gap', { channel: 'thread:wakes_changed' }, '');
    await vi.advanceTimersByTimeAsync(500);
    expect(gapRead).toHaveBeenCalledTimes(1);
    expect(controller.count).toBe(0);
  });

  it('bounds a burst of changes to a few reads', async () => {
    const { pane, read } = await mountWakes([row('tests')]);
    for (let i = 0; i < 50; i++) {
      emitWailsEvent('thread:wakes_changed', { threadId: pane.threadId, id: 'wake-1' });
      await vi.advanceTimersByTimeAsync(20);
    }
    await vi.advanceTimersByTimeAsync(500);
    // 1s of frames every 20ms: the 400ms ceiling allows about three.
    expect(read.mock.calls.length).toBeGreaterThan(1);
    expect(read.mock.calls.length).toBeLessThanOrEqual(5);
  });

  it('keeps the last list through a failed read', async () => {
    const { pane, controller } = await mountWakes([row('tests')]);
    const log = vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('ListThreadWakes', async () => { throw new Error('read failed'); });
    emitWailsEvent('thread:wakes_changed', { threadId: pane.threadId, id: 'wake-1' });
    await vi.advanceTimersByTimeAsync(500);
    expect(log).toHaveBeenCalled();
    expect(controller.rows.map((r) => r.id)).toEqual(['tests']);
  });

  it('reads again when its computer reconnects', async () => {
    const { read } = await mountWakes([row('tests')]);
    __setBackendStatusForTest('', { status: 'reconnecting', nextAttemptAt: null });
    __setBackendStatusForTest('', { status: 'connected', nextAttemptAt: null });
    await vi.advanceTimersByTimeAsync(0);
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('clears and reads nothing for a draft placeholder', async () => {
    const { pane, read, controller } = await mountWakes([row('tests')]);
    expect(controller.count).toBe(1);
    pane.startDraftPlaceholder({ id: 'proj', name: 'proj', path: '/proj', createdAt: 0, updatedAt: 0 } as Project);
    await vi.advanceTimersByTimeAsync(0);
    expect(controller.threadId).toBeNull();
    expect(controller.count).toBe(0);
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('stops listening when unmounted', async () => {
    const { pane, read } = await mountWakes([row('tests')]);
    release();
    release = () => {};
    emitWailsEvent('thread:wakes_changed', { threadId: pane.threadId, id: 'wake-1' });
    emitWailsEvent('transport:gap', { channel: 'thread:wakes_changed' }, '');
    await vi.advanceTimersByTimeAsync(500);
    expect(read).toHaveBeenCalledTimes(1);
  });
});
