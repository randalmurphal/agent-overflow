import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { tick, flushSync } from 'svelte';
import { createThreadPaneScroll } from './threadPaneScroll.svelte';
import { stubScrollController } from '../../test/helpers/chat';

let frames: Map<number, FrameRequestCallback>;
beforeEach(() => {
  vi.useFakeTimers();
  frames = new Map();
  let next = 0;
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
    frames.set(++next, cb);
    return next;
  });
  vi.stubGlobal('cancelAnimationFrame', (id: number) => frames.delete(id));
});
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

function fixture() {
  const state = { loading: false, generation: 0 };
  const controller = stubScrollController();
  const observe = vi.spyOn(controller, 'observe');
  const mark = vi.spyOn(controller, 'markStructuralContentPending');
  const scroll = createThreadPaneScroll({
    getThread: () => null, getLoading: () => state.loading,
    getSwitchGeneration: () => state.generation, getItemCount: () => 10,
    stampLiveContent: () => {},
  });
  scroll.attach(controller);
  return { scroll, controller, observe, mark, state };
}

it('coalesces a burst into one post-flush frame while arming every append synchronously', async () => {
  const { scroll, controller, observe, mark } = fixture();
  for (let i = 0; i < 500; i++) scroll.armStructuralSpring();
  expect(mark).toHaveBeenCalledTimes(500);
  expect(frames.size).toBe(0);
  await tick();
  expect(frames.size).toBe(1);
  expect(vi.getTimerCount()).toBe(1);
  for (const callback of [...frames.values()]) callback(16);
  expect(observe).toHaveBeenCalledExactlyOnceWith('live-content');
  expect(frames.size).toBe(0);
  expect(vi.getTimerCount()).toBe(0);
  scroll.detach(controller);
});

it.each(['flush', 'frame'] as const)('detach releases a nudge during its %s boundary', async (boundary) => {
  const { scroll, controller, observe } = fixture();
  scroll.armStructuralSpring();
  if (boundary === 'frame') await tick();
  const stale = [...frames.values()];
  scroll.detach(controller);
  scroll.detach(controller);
  await tick();
  for (const callback of stale) callback(16);
  expect(frames.size).toBe(0);
  expect(vi.getTimerCount()).toBe(0);
  expect(observe).not.toHaveBeenCalled();
});

it('supersedes the frame, clears an opted-out nudge, and supports opting back in', async () => {
  const { scroll, controller, observe, state } = fixture();
  scroll.armStructuralSpring();
  await tick();
  const stale = [...frames.values()];
  state.loading = true;
  expect(scroll.armStructuralSpring()).toBe(false);
  expect(frames.size).toBe(0);
  state.loading = false;
  scroll.armStructuralSpring();
  await tick();
  for (const callback of stale) callback(16);
  expect(observe).not.toHaveBeenCalled();
  expect(frames.size).toBe(1);
  await vi.advanceTimersByTimeAsync(32);
  expect(observe).toHaveBeenCalledExactlyOnceWith('live-content');
  expect(frames.size).toBe(0);
  expect(vi.getTimerCount()).toBe(0);
  scroll.detach(controller);
});

it('a replaced controller and a changed thread generation cannot receive stale nudges', async () => {
  const { scroll, controller, observe, state } = fixture();
  scroll.armStructuralSpring();
  await tick();
  const replacement = stubScrollController();
  const replacementObserve = vi.spyOn(replacement, 'observe');
  scroll.attach(replacement);
  scroll.detach(controller);
  scroll.armStructuralSpring();
  await tick();
  state.generation += 1;
  await vi.advanceTimersByTimeAsync(32);
  expect(observe).not.toHaveBeenCalled();
  expect(replacementObserve).not.toHaveBeenCalled();
  scroll.armStructuralSpring();
  await tick();
  await vi.advanceTimersByTimeAsync(32);
  expect(replacementObserve).toHaveBeenCalledExactlyOnceWith('live-content');
  scroll.detach(replacement);
});

it('registration does not subscribe a mounting effect to the controller slot it writes', () => {
  const { scroll } = fixture();
  let runs = 0;
  const dispose = $effect.root(() => {
    $effect(() => {
      runs += 1;
      scroll.attach(stubScrollController());
    });
  });
  try {
    flushSync();
    expect(runs).toBe(1);
    const controller = stubScrollController();
    scroll.attach(controller);
    flushSync();
    expect(runs).toBe(1);
    scroll.detach(controller);
  } finally {
    dispose();
  }
});
