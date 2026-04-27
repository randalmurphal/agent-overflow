import { describe, expect, it } from 'vitest';
import { scrollDeltaForMeasuredRowChange } from './scrollAnchor';

describe('scrollDeltaForMeasuredRowChange', () => {
  it('preserves the viewport anchor when a fully above-viewport row grows', () => {
    expect(scrollDeltaForMeasuredRowChange({
      previousHeight: 100,
      nextHeight: 260,
      rowBottom: 40,
      viewportTop: 100,
    })).toBe(160);
  });

  it('preserves the viewport anchor when an above-viewport row grows into the viewport', () => {
    expect(scrollDeltaForMeasuredRowChange({
      previousHeight: 100,
      nextHeight: 260,
      rowBottom: 140,
      viewportTop: 100,
    })).toBe(160);
  });

  it('preserves the viewport anchor when a fully above-viewport row shrinks', () => {
    expect(scrollDeltaForMeasuredRowChange({
      previousHeight: 260,
      nextHeight: 100,
      rowBottom: -120,
      viewportTop: 100,
    })).toBe(-160);
  });

  it('does not adjust the initial measurement or rows that were inside the viewport', () => {
    expect(scrollDeltaForMeasuredRowChange({
      previousHeight: 0,
      nextHeight: 260,
      rowBottom: 40,
      viewportTop: 100,
    })).toBe(0);

    expect(scrollDeltaForMeasuredRowChange({
      previousHeight: 100,
      nextHeight: 260,
      rowBottom: 280,
      viewportTop: 100,
    })).toBe(0);
  });
});
