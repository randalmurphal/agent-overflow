import { describe, expect, it } from 'vitest';

import { MODE_CYCLE, cycleMode } from './modeCycle';

describe('cycleMode', () => {
  it.each([
    ['advances', 'chat', 'plan'],
    ['wraps', 'plan', 'chat'],
  ])('%s %s → %s', (_name, from, to) => {
    expect(cycleMode(from)).toBe(to);
  });

  it('falls back to chat on unknown mode', () => {
    // Saga-owned and unknown thread types are intentionally outside the cycle.
    // Pressing Shift+Tab on one should return chat rather than crash.
    expect(cycleMode('legacy')).toBe('chat');
    expect(cycleMode('default')).toBe('chat');
    expect(cycleMode('')).toBe('chat');
    expect(cycleMode(undefined)).toBe('chat');
    expect(cycleMode(null)).toBe('chat');
  });

  it('exposes the cycle tuple in order', () => {
    expect(MODE_CYCLE).toEqual(['chat', 'plan']);
  });
});
