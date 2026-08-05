import { beforeEach, describe, expect, it } from 'vitest';
import {
  applyCompactingState,
  clearCompactingForThread,
  hydrateCompactingState,
  isThreadCompacting,
  resetForTest,
} from './compactingState.svelte';

describe('compactingState', () => {
  beforeEach(() => {
    resetForTest();
  });

  it('starts not-compacting for every thread', () => {
    expect(isThreadCompacting('t1')).toBe(false);
    expect(isThreadCompacting(null)).toBe(false);
  });

  it('sets on an active frame and drops on the close frame', () => {
    applyCompactingState({ threadId: 't1', active: true, sinceUnixMs: 1_754_000_000_000 });
    expect(isThreadCompacting('t1')).toBe(true);
    expect(isThreadCompacting('t2')).toBe(false);

    applyCompactingState({ threadId: 't1', active: false });
    expect(isThreadCompacting('t1')).toBe(false);
  });

  it('treats an active frame without sinceUnixMs as compacting', () => {
    applyCompactingState({ threadId: 't1', active: true });
    expect(isThreadCompacting('t1')).toBe(true);
  });

  it('ignores malformed frames', () => {
    applyCompactingState(undefined);
    applyCompactingState({ threadId: '', active: true });
    expect(isThreadCompacting('')).toBe(false);
  });

  // Refresh mid-window: the snapshot is the only source (the window spans
  // minutes of wire silence), and a 0 must clear a flag whose close frame
  // was missed while disconnected.
  it('hydrates both directions from the live-state snapshot', () => {
    hydrateCompactingState('t1', 1_754_000_000_000);
    expect(isThreadCompacting('t1')).toBe(true);

    hydrateCompactingState('t1', 0);
    expect(isThreadCompacting('t1')).toBe(false);
  });

  it('drops a thread on clear', () => {
    applyCompactingState({ threadId: 't1', active: true, sinceUnixMs: 1 });
    clearCompactingForThread('t1');
    expect(isThreadCompacting('t1')).toBe(false);
  });
});
