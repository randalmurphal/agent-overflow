import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { setBindingMock, getBindingMock } from '../../test/mocks/bindings-app';
import {
  startIdleMemoryTrim,
  IDLE_TRIM_THRESHOLD_MS,
  IDLE_TRIM_REATTEMPT_MS,
  IDLE_TRIM_CHECK_MS,
} from './idleMemoryTrim';

// Drives the detector through fake time. The mocked Date.now advances with
// the timers, so "idle" is simply not dispatching input events while time
// passes.
describe('idleMemoryTrim', () => {
  let stop: (() => void) | null = null;

  beforeEach(() => {
    vi.useFakeTimers();
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
