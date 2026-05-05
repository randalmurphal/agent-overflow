import { describe, expect, it } from 'vitest';

import { MODE_CYCLE, cycleMode } from './modeCycle';

describe('cycleMode', () => {
  it('advances chat → plan', () => {
    expect(cycleMode('chat')).toBe('plan');
  });

  it('wraps plan → chat', () => {
    expect(cycleMode('plan')).toBe('chat');
  });

  it('falls back to chat on unknown mode', () => {
    // design and discussion are immutable thread types — they are
    // intentionally outside the cycle. Pressing Shift+Tab on one of those
    // threads should return chat (not crash); the backend additionally
    // rejects design ↔ chat / plan transitions on the row.
    expect(cycleMode('design')).toBe('chat');
    expect(cycleMode('discussion')).toBe('chat');
    expect(cycleMode('default')).toBe('chat');
    expect(cycleMode('')).toBe('chat');
    expect(cycleMode(undefined)).toBe('chat');
    expect(cycleMode(null)).toBe('chat');
  });

  it('exposes the cycle tuple in order', () => {
    expect(MODE_CYCLE).toEqual(['chat', 'plan']);
  });
});
