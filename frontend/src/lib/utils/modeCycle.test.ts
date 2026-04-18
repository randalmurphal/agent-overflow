import { describe, expect, it } from 'vitest';

import { MODE_CYCLE, cycleMode } from './modeCycle';

describe('cycleMode', () => {
  it('advances chat → plan', () => {
    expect(cycleMode('chat')).toBe('plan');
  });

  it('advances plan → design', () => {
    expect(cycleMode('plan')).toBe('design');
  });

  it('wraps design → chat', () => {
    expect(cycleMode('design')).toBe('chat');
  });

  it('falls back to chat on unknown mode', () => {
    // discussion is outside the cycle — pressing Shift+Tab on a
    // discussion-root thread should return the thread to a cycleable
    // state rather than crash.
    expect(cycleMode('discussion')).toBe('chat');
    expect(cycleMode('default')).toBe('chat');
    expect(cycleMode('')).toBe('chat');
    expect(cycleMode(undefined)).toBe('chat');
    expect(cycleMode(null)).toBe('chat');
  });

  it('exposes the cycle tuple in order', () => {
    expect(MODE_CYCLE).toEqual(['chat', 'plan', 'design']);
  });
});
