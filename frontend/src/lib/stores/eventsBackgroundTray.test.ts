import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { setupEventListeners } from './events';
import { onBackgroundTrayEvent } from './eventsBackgroundTray';
import { resetBindingMocks } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import type { BackgroundTrayEvent } from '../types/events';

let cleanupEvents: (() => void) | null = null;
let received: BackgroundTrayEvent[] = [];
let unsubscribe: (() => void) | null = null;

beforeEach(() => {
  resetBindingMocks();
  received = [];
  unsubscribe = onBackgroundTrayEvent((evt) => received.push(evt));
  cleanupEvents = setupEventListeners();
});

afterEach(() => {
  cleanupEvents?.();
  cleanupEvents = null;
  unsubscribe?.();
  unsubscribe = null;
});

describe('provider:background_tray routing', () => {
  it('hands each frame with a thread to the tray subscribers', () => {
    const delta: BackgroundTrayEvent = { threadId: 't1', launchIds: ['a'], rows: [] };
    emitWailsEvent('provider:background_tray', delta);
    emitWailsEvent('provider:background_tray', { threadId: 't2', refresh: true });
    expect(received).toEqual([delta, { threadId: 't2', refresh: true }]);
  });

  it('drops a frame that names no thread', () => {
    emitWailsEvent('provider:background_tray', { launchIds: ['a'] });
    emitWailsEvent('provider:background_tray', { threadId: '', refresh: true });
    emitWailsEvent('provider:background_tray', undefined);
    expect(received).toEqual([]);
  });

  it('stops routing once the listeners are torn down or the subscriber leaves', () => {
    cleanupEvents?.();
    cleanupEvents = null;
    emitWailsEvent('provider:background_tray', { threadId: 't1', refresh: true });
    expect(received).toEqual([]);

    cleanupEvents = setupEventListeners();
    unsubscribe?.();
    unsubscribe = null;
    emitWailsEvent('provider:background_tray', { threadId: 't1', refresh: true });
    expect(received).toEqual([]);
  });
});
