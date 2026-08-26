import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { setBindingMock, getBindingMock } from '../../test/mocks/bindings-app';
import {
  startIdleMemoryTrim,
  IDLE_TRIM_THRESHOLD_MS,
  IDLE_TRIM_REATTEMPT_MS,
  IDLE_TRIM_CHECK_MS,
} from './idleMemoryTrim';

// The drain gate reads the pane registry through revealDrainProbe; tests
// drive it with a knob instead of mounting panes.
let drainingPanes = 0;
vi.mock('./revealDrainProbe', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./revealDrainProbe')>()),
  revealDrainStats: () =>
    Promise.resolve({ v: 1 as const, panes: 1, draining: drainingPanes, smoothers: 0, boundaries: 0 }),
}));

// Drives the detector through fake time. The mocked Date.now advances with
// the timers, so "idle" is simply not dispatching input events while time
// passes.
describe('idleMemoryTrim', () => {
  let stop: (() => void) | null = null;

  beforeEach(() => {
    vi.useFakeTimers();
    drainingPanes = 0;
    setBindingMock('RequestWebviewMemoryTrim', () => Promise.resolve('requested'));
  });

  afterEach(() => {
    stop?.();
    stop = null;
    vi.useRealTimers();
  });

  const trimCalls = () => getBindingMock('RequestWebviewMemoryTrim')!.mock.calls.length;

  it('requests a trim once input has been idle past the threshold', async () => {
    stop = startIdleMemoryTrim();
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS - IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(0);
    await vi.advanceTimersByTimeAsync(2 * IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(1);
  });

  it('input resets the idle clock', async () => {
    stop = startIdleMemoryTrim();
    // Keep touching the keyboard just before every check would fire.
    for (let i = 0; i < 10; i++) {
      await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS - IDLE_TRIM_CHECK_MS);
      window.dispatchEvent(new Event('keydown'));
    }
    expect(trimCalls()).toBe(0);
  });

  it('holds the reattempt floor while idleness persists', async () => {
    stop = startIdleMemoryTrim();
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS + IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(1);
    // Stay idle: the next request waits for the floor, not the next check.
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_REATTEMPT_MS - 2 * IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(1);
    await vi.advanceTimersByTimeAsync(2 * IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(2);
  });

  it('reports whether input landed since the last accepted trim', async () => {
    stop = startIdleMemoryTrim();
    const mock = getBindingMock('RequestWebviewMemoryTrim')!;

    // First ask since page load: the boot render was work.
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS + IDLE_TRIM_CHECK_MS);
    expect(mock.mock.calls[0]).toEqual([true]);

    // Idle straight through the floor: nothing new for a GC to reclaim,
    // and the backend's no-activity gate keys on this false.
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_REATTEMPT_MS);
    expect(mock.mock.calls.length).toBe(2);
    expect(mock.mock.calls[1]).toEqual([false]);

    // Fresh input reopens the caller's half of the gate.
    window.dispatchEvent(new Event('keydown'));
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_REATTEMPT_MS + IDLE_TRIM_THRESHOLD_MS);
    expect(mock.mock.calls.at(-1)).toEqual([true]);
  });

  it('a draining pane holds the trim, and it fires on the next check, not a floor later', async () => {
    drainingPanes = 1;
    stop = startIdleMemoryTrim();
    // Idle well past the threshold: the drain alone is what holds it.
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS + 4 * IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(0);
    // Drain empties: the very next 5s check fires — a drain skip must not
    // stamp the reattempt floor.
    drainingPanes = 0;
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(1);
  });

  it('a transient RPC failure stays armed and retries after the floor', async () => {
    let calls = 0;
    setBindingMock('RequestWebviewMemoryTrim', () => {
      calls++;
      return calls === 1
        ? Promise.reject(Object.assign(new Error('rpc timeout'), { code: 'timeout' }))
        : Promise.resolve('requested');
    });
    stop = startIdleMemoryTrim();
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS + IDLE_TRIM_REATTEMPT_MS + 2 * IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(2);
  });

  it('disarms permanently when the method is unavailable', async () => {
    setBindingMock('RequestWebviewMemoryTrim', () =>
      Promise.reject(Object.assign(new Error('no such method'), { code: 'method_not_found' })),
    );
    stop = startIdleMemoryTrim();
    await vi.advanceTimersByTimeAsync(IDLE_TRIM_THRESHOLD_MS + IDLE_TRIM_CHECK_MS);
    expect(trimCalls()).toBe(1);
    await vi.advanceTimersByTimeAsync(4 * IDLE_TRIM_REATTEMPT_MS);
    expect(trimCalls()).toBe(1);
  });

  it('stop removes the timer', async () => {
    stop = startIdleMemoryTrim();
    stop();
    stop = null;
    await vi.advanceTimersByTimeAsync(4 * IDLE_TRIM_THRESHOLD_MS);
    expect(trimCalls()).toBe(0);
  });
});
