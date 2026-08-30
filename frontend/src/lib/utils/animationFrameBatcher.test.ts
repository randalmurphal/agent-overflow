import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetAnimationFrameCoordinatorForTest,
  createAnimationFrameBatcher,
} from './animationFrameBatcher';

describe('animation frame coordination', () => {
  let frames: FrameRequestCallback[];
  let cancelled: number[];

  beforeEach(() => {
    frames = [];
    cancelled = [];
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      frames.push(callback);
      return frames.length;
    });
    vi.stubGlobal('cancelAnimationFrame', (handle: number) => {
      cancelled.push(handle);
    });
    __resetAnimationFrameCoordinatorForTest();
    frames = [];
    cancelled = [];
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('uses one native callback and runs geometry before DOM-update owners', () => {
    const order: string[] = [];
    const reveal = createAnimationFrameBatcher('test-reveal');
    const spring = createAnimationFrameBatcher('test-spring', 'before-dom-update');

    reveal.request(() => {
      order.push('reveal');
    });
    spring.request((timestamp) => order.push(`spring:${timestamp}`));

    expect(frames).toHaveLength(1);
    frames.shift()?.(42);
    expect(order).toEqual(['spring:42', 'reveal']);
  });

  it('keeps work requested during dispatch on the next native frame', () => {
    const order: string[] = [];
    const update = createAnimationFrameBatcher('test-update');
    const geometry = createAnimationFrameBatcher('test-geometry', 'before-dom-update');

    update.request(() => {
      order.push('update-1');
      update.request(() => order.push('update-2'));
      geometry.request(() => order.push('geometry-2'));
    });
    geometry.request(() => order.push('geometry-1'));

    expect(frames).toHaveLength(1);
    frames.shift()?.(10);
    expect(order).toEqual(['geometry-1', 'update-1']);
    expect(frames).toHaveLength(1);

    frames.shift()?.(20);
    expect(order).toEqual(['geometry-1', 'update-1', 'geometry-2', 'update-2']);
  });

  it('cancels one owner without cancelling another phase', () => {
    const order: string[] = [];
    const update = createAnimationFrameBatcher('test-update');
    const geometry = createAnimationFrameBatcher('test-geometry', 'before-dom-update');
    const updateHandle = update.request(() => order.push('update'));
    geometry.request(() => order.push('geometry'));

    update.cancel(updateHandle);
    expect(cancelled).toEqual([]);
    expect(frames).toHaveLength(1);
    frames.shift()?.(7);
    expect(order).toEqual(['geometry']);
  });

  it('cancels the native frame when the final pending owner cancels', () => {
    const update = createAnimationFrameBatcher('test-update');
    const callback = vi.fn();
    const handle = update.request(callback);

    update.cancel(handle);

    expect(cancelled).toEqual([1]);
    // The test stub retains the cancelled closure. Even if a broken host were
    // to invoke it, the coordinator has already removed the owner.
    frames.shift()?.(11);
    expect(callback).not.toHaveBeenCalled();

    update.request(callback);
    expect(frames).toHaveLength(1);
    frames.shift()?.(12);
    expect(callback).toHaveBeenCalledOnce();
  });

  it('honors cancellation from an earlier callback in the same phase', () => {
    const order: string[] = [];
    const update = createAnimationFrameBatcher('test-update');
    let laterHandle = 0;
    update.request(() => {
      order.push('first');
      update.cancel(laterHandle);
    });
    laterHandle = update.request(() => order.push('later'));

    frames.shift()?.(20);
    expect(order).toEqual(['first']);
  });

  it('finishes both phases, reports secondary failures, and rethrows the first', () => {
    const order: string[] = [];
    const firstError = new Error('geometry failed');
    const secondError = new Error('update failed');
    const errorLog = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const geometry = createAnimationFrameBatcher('test-geometry', 'before-dom-update');
    const update = createAnimationFrameBatcher('test-update');

    geometry.request(() => {
      order.push('geometry-failure');
      throw firstError;
    });
    geometry.request(() => order.push('geometry-after-failure'));
    update.request(() => {
      order.push('update-failure');
      throw secondError;
    });
    update.request(() => order.push('update-after-failure'));

    expect(() => frames.shift()?.(30)).toThrow(firstError);
    expect(order).toEqual([
      'geometry-failure',
      'geometry-after-failure',
      'update-failure',
      'update-after-failure',
    ]);
    expect(errorLog).toHaveBeenCalledWith(
      '[test-update] animation-frame callback failed',
      secondError,
    );
  });

  it('rolls back a callback when native frame registration fails', () => {
    const update = createAnimationFrameBatcher('test-update');
    const registrationError = new Error('requestAnimationFrame failed');
    vi.stubGlobal('requestAnimationFrame', () => {
      throw registrationError;
    });

    expect(() => update.request(() => undefined)).toThrow(registrationError);

    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      frames.push(callback);
      return 99;
    });
    const callback = vi.fn();
    update.request(callback);
    expect(frames).toHaveLength(1);
    frames.shift()?.(40);
    expect(callback).toHaveBeenCalledOnce();
  });
});
