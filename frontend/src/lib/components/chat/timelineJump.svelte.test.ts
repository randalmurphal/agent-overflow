// The landing flash waits out the jump's multi-frame scroll
// convergence before reading geometry — flashing from the frame the
// RPC resolved read a pre-jump offset and silently never showed
// (found live 2026-08-19). These tests drive the rAF watch by hand.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import { createTimelineJump, type TimelineJump, type TimelineJumpOptions } from './timelineJump.svelte';

interface FakeList {
  offset: number;
  itemOffset: number;
  size: number;
  viewport: number;
}

function makeListRef(fake: FakeList): TimelineVirtualizerHandle {
  return {
    getScrollOffset: () => fake.offset,
    getViewportSize: () => fake.viewport,
    getItemOffset: () => fake.itemOffset,
    sizeAt: () => fake.size,
    findItemIndex: () => 0,
  } as unknown as TimelineVirtualizerHandle;
}

describe('timelineJump landing flash', () => {
  let frames: FrameRequestCallback[];
  let jump: TimelineJump;
  let fake: FakeList;
  let pane: { threadId: string | null };
  let scrollToItemResult: boolean;

  function drainFrame(): void {
    const batch = frames;
    frames = [];
    for (const cb of batch) cb(0);
  }

  function makeJump(overrides: Partial<TimelineJumpOptions> = {}): TimelineJump {
    const listRef = makeListRef(fake);
    return createTimelineJump({
      getPane: () => pane as ThreadPane,
      getListRef: () => listRef,
      scrollToItem: async () => scrollToItemResult,
      findTimelineNodeIndex: () => 0,
      ...overrides,
    });
  }

  beforeEach(() => {
    frames = [];
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      frames.push(cb);
      return frames.length;
    });
    vi.stubGlobal('cancelAnimationFrame', () => {
      frames = [];
    });
    fake = { offset: 0, itemOffset: 100, size: 200, viewport: 800 };
    pane = { threadId: 't1' };
    scrollToItemResult = true;
    jump = makeJump();
  });

  afterEach(() => {
    jump.invalidate();
    vi.unstubAllGlobals();
  });

  it('sets the flash only after the offset holds still with the target in view', async () => {
    await jump.jumpToItem('u1');
    expect(jump.flash).toBeNull();
    // Converging: the offset moves for two frames.
    fake.offset = 400;
    drainFrame();
    fake.offset = 900;
    fake.itemOffset = 1000;
    drainFrame();
    expect(jump.flash).toBeNull();
    // Settled: two consecutive frames at the same offset.
    drainFrame();
    expect(jump.flash).toBeNull();
    drainFrame();
    expect(jump.flash).not.toBeNull();
    expect(jump.flash?.top).toBe(100);
    expect(jump.flash?.height).toBe(200);
  });

  it('gives up without a flash when the target never lands in view', async () => {
    fake.itemOffset = 50_000;
    await jump.jumpToItem('u1');
    for (let i = 0; i < 60 && frames.length > 0; i++) drainFrame();
    expect(jump.flash).toBeNull();
    expect(frames.length).toBe(0);
  });

  it('clamps a taller-than-viewport landing to the viewport rect', async () => {
    fake.itemOffset = -100;
    fake.size = 5000;
    await jump.jumpToItem('u1');
    drainFrame();
    drainFrame();
    drainFrame();
    expect(jump.flash?.top).toBe(0);
    expect(jump.flash?.height).toBe(800);
  });

  it('a refused jump arms no watch and cannot flash', async () => {
    scrollToItemResult = false;
    await jump.jumpToItem('u1');
    expect(frames.length).toBe(0);
    expect(jump.flash).toBeNull();
  });

  it('a thread switch mid-watch aborts without a flash', async () => {
    await jump.jumpToItem('u1');
    drainFrame();
    pane.threadId = 't2';
    drainFrame();
    drainFrame();
    drainFrame();
    expect(jump.flash).toBeNull();
    expect(frames.length).toBe(0);
  });

  it('invalidate cancels an in-flight watch', async () => {
    await jump.jumpToItem('u1');
    expect(frames.length).toBe(1);
    jump.invalidate();
    expect(frames.length).toBe(0);
    expect(jump.flash).toBeNull();
  });

  it('a reader scroll past the tolerance cancels the flash; a nudge does not', async () => {
    await jump.jumpToItem('u1');
    drainFrame();
    drainFrame();
    drainFrame();
    expect(jump.flash).not.toBeNull();
    jump.noteScroll(fake.offset + 20);
    expect(jump.flash).not.toBeNull();
    jump.noteScroll(fake.offset + 200);
    expect(jump.flash).toBeNull();
  });
});
