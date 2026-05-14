import { afterEach, describe, expect, it, vi } from 'vitest';
import { createTimelineTargetFlash } from './timelineTargetFlash.svelte';

describe('createTimelineTargetFlash', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('marks an item as flashing and clears it after the timeout', () => {
    vi.useFakeTimers();
    const flash = createTimelineTargetFlash(900);

    flash.flash('item-1');

    expect(flash.itemId).toBe('item-1');
    expect(flash.nonce).toBe(1);

    vi.advanceTimersByTime(899);
    expect(flash.itemId).toBe('item-1');

    vi.advanceTimersByTime(1);
    expect(flash.itemId).toBeNull();
  });

  it('bumps the nonce and cancels the previous clear timer when flashing a new item', () => {
    vi.useFakeTimers();
    const flash = createTimelineTargetFlash(900);

    flash.flash('item-1');
    vi.advanceTimersByTime(400);
    flash.flash('item-2');

    expect(flash.itemId).toBe('item-2');
    expect(flash.nonce).toBe(2);

    vi.advanceTimersByTime(500);
    expect(flash.itemId).toBe('item-2');

    vi.advanceTimersByTime(400);
    expect(flash.itemId).toBeNull();
  });

  it('clears the active item and pending timer', () => {
    vi.useFakeTimers();
    const flash = createTimelineTargetFlash(900);

    flash.flash('item-1');
    flash.clear();
    vi.advanceTimersByTime(900);

    expect(flash.itemId).toBeNull();
    expect(flash.nonce).toBe(1);
  });
});
