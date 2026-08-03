import { beforeEach, describe, expect, it } from 'vitest';
import {
  applyFastModeState,
  clearFastModeStateForThread,
  getFastModeReport,
  resetForTest,
} from './fastModeState.svelte';

describe('fastModeState', () => {
  beforeEach(() => {
    resetForTest();
  });

  it('starts unknown for every thread', () => {
    expect(getFastModeReport('t1')).toBeUndefined();
    expect(getFastModeReport(null)).toBeUndefined();
  });

  it('stores the latest report per thread', () => {
    applyFastModeState({ threadId: 't1', state: 'off', disabledReason: 'free' });
    applyFastModeState({ threadId: 't2', state: 'on' });

    expect(getFastModeReport('t1')).toEqual({ state: 'off', disabledReason: 'free' });
    expect(getFastModeReport('t2')).toEqual({ state: 'on', disabledReason: '' });
  });

  it('replaces rather than merges — the newest frame is the whole answer', () => {
    applyFastModeState({ threadId: 't1', state: 'off', disabledReason: 'network_error' });
    applyFastModeState({ threadId: 't1', state: 'on' });

    expect(getFastModeReport('t1')).toEqual({ state: 'on', disabledReason: '' });
  });

  // An all-empty frame would flip the thread from "unknown" to a report
  // whose falsy state reads as a denial downstream.
  it('ignores a frame that carries no signal', () => {
    applyFastModeState({ threadId: 't1', state: '', disabledReason: '  ' });
    expect(getFastModeReport('t1')).toBeUndefined();
  });

  it('ignores malformed frames', () => {
    applyFastModeState(undefined);
    applyFastModeState({ threadId: '', state: 'on' });
    expect(getFastModeReport('')).toBeUndefined();
  });

  it('drops a thread on clear', () => {
    applyFastModeState({ threadId: 't1', state: 'cooldown' });
    clearFastModeStateForThread('t1');
    expect(getFastModeReport('t1')).toBeUndefined();
  });
});
