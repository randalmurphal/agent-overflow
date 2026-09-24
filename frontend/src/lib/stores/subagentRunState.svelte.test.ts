import { afterEach, describe, expect, it } from 'vitest';
import {
  clearSubagentRunStatesForThread,
  liveSubagentRunState,
  replaceSubagentRunStates,
  resetForTest,
} from './subagentRunState.svelte';
import type { SubagentRunState } from '../utils/subagentRunState';

const running: SubagentRunState = { state: 'running', waitingOn: 0, report: null };
const parked: SubagentRunState = { state: 'parked', waitingOn: 2, report: { id: 'r1', preview: 'Found it.' } };

describe('subagentRunState store', () => {
  afterEach(() => resetForTest());

  it('serves a thread’s states by launch id and drops launches a later read no longer lists', () => {
    replaceSubagentRunStates('t1', new Map([['a', running], ['b', parked]]));
    expect(liveSubagentRunState('t1', 'a')).toEqual(running);
    expect(liveSubagentRunState('t1', 'b')).toEqual(parked);
    expect(liveSubagentRunState('t1', 'c')).toBeNull();
    expect(liveSubagentRunState('t2', 'a')).toBeNull();

    replaceSubagentRunStates('t1', new Map([['b', running]]));
    expect(liveSubagentRunState('t1', 'a')).toBeNull();
    expect(liveSubagentRunState('t1', 'b')).toEqual(running);
  });

  it('keeps the same object for an unchanged state so readers do not wake, and replaces a changed one', () => {
    replaceSubagentRunStates('t1', new Map([['a', parked]]));
    const before = liveSubagentRunState('t1', 'a');
    replaceSubagentRunStates('t1', new Map([['a', { ...parked, report: { ...parked.report! } }]]));
    expect(liveSubagentRunState('t1', 'a')).toBe(before);

    replaceSubagentRunStates('t1', new Map([['a', { ...parked, waitingOn: 1 }]]));
    expect(liveSubagentRunState('t1', 'a')).not.toBe(before);
    expect(liveSubagentRunState('t1', 'a')?.waitingOn).toBe(1);
  });

  it('keeps threads apart and clears one thread on teardown', () => {
    replaceSubagentRunStates('t1', new Map([['a', running]]));
    replaceSubagentRunStates('t2', new Map([['a', parked]]));
    clearSubagentRunStatesForThread('t1');
    expect(liveSubagentRunState('t1', 'a')).toBeNull();
    expect(liveSubagentRunState('t2', 'a')).toEqual(parked);

    replaceSubagentRunStates('t2', new Map());
    expect(liveSubagentRunState('t2', 'a')).toBeNull();
    clearSubagentRunStatesForThread('t2');
    clearSubagentRunStatesForThread('');
  });

  it('answers null for a missing thread or launch id', () => {
    expect(liveSubagentRunState(null, 'a')).toBeNull();
    expect(liveSubagentRunState('t1', undefined)).toBeNull();
    replaceSubagentRunStates('', new Map([['a', running]]));
    expect(liveSubagentRunState('', 'a')).toBeNull();
  });
});
