import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { setupEventListeners } from './events';
import { onThreadWakesChanged, type ThreadWakeChange } from './eventsThreadWakes';
import { resetBindingMocks } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { __attachBackendForTest, detachBackend } from '../transport/backends';

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

let cleanupEvents: (() => void) | null = null;
let received: [ThreadWakeChange | null, string][] = [];
let unsubscribe: (() => void) | null = null;

beforeEach(() => {
  resetBindingMocks();
  attachRemote();
  received = [];
  unsubscribe = onThreadWakesChanged((change, backend) => received.push([change, backend]));
  cleanupEvents = setupEventListeners();
});

afterEach(() => {
  cleanupEvents?.();
  cleanupEvents = null;
  unsubscribe?.();
  unsubscribe = null;
  detachBackend(remote);
});

describe('thread:wakes_changed routing', () => {
  it('hands each frame its change and computer, and a lost frame its computer alone', () => {
    const row = { id: 'w1', kind: 'loop', label: 'board', state: 'armed', nextAt: 5 };
    emitWailsEvent('thread:wakes_changed', { threadId: 't1', id: 'w1', row });
    emitWailsEvent('thread:wakes_changed', { threadId: 't2', id: 'w2', removed: true }, remote);
    emitWailsEvent('thread:wakes_changed', { threadId: 't3', id: 'w3' }, remote);
    emitWailsEvent('transport:gap', { channel: 'thread:wakes_changed' }, remote);
    expect(received).toEqual([
      [{ threadId: 't1', id: 'w1', row }, ''],
      [{ threadId: 't2', id: 'w2', removed: true }, remote],
      [{ threadId: 't3', id: 'w3' }, remote],
      [null, remote],
    ]);
  });

  it('drops a frame that names no thread or no row id', () => {
    emitWailsEvent('thread:wakes_changed', {});
    emitWailsEvent('thread:wakes_changed', { threadId: '', id: 'w1' });
    emitWailsEvent('thread:wakes_changed', { threadId: 't1' });
    emitWailsEvent('thread:wakes_changed', undefined);
    expect(received).toEqual([]);
  });

  it('stops routing once the listeners are torn down or the subscriber leaves', () => {
    cleanupEvents?.();
    cleanupEvents = null;
    emitWailsEvent('thread:wakes_changed', { threadId: 't1', id: 'w1' });
    expect(received).toEqual([]);

    cleanupEvents = setupEventListeners();
    unsubscribe?.();
    unsubscribe = null;
    emitWailsEvent('thread:wakes_changed', { threadId: 't1', id: 'w1' });
    expect(received).toEqual([]);
  });
});
