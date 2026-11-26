// Which provider Ask your agent starts on, so its agent gets the tracker
// tools, and why Ask cannot run when no provider turned on has them.

import { describe, expect, it } from 'vitest';
import { trackerToolsBlocked, trackerToolsProvider } from './trackerTools';

const on = { claudeEnabled: true, codexEnabled: true, claudeTuiEnabled: true, agyEnabled: true };

describe('the provider Ask starts on', () => {
  it('is the default when its agents get the tracker tools', () => {
    expect(trackerToolsProvider('codex', on)).toBe('codex');
    expect(trackerToolsProvider('claude', on)).toBe('claude');
  });

  it('is headless Claude, else Codex, for a default without them', () => {
    expect(trackerToolsProvider('claude-tui', on)).toBe('claude');
    expect(trackerToolsProvider('agy', on)).toBe('claude');
    expect(trackerToolsProvider('agy', { ...on, claudeEnabled: false })).toBe('codex');
    expect(trackerToolsProvider('codex', { ...on, codexEnabled: false })).toBe('claude');
  });

  it('is none when no provider turned on has them, and Ask says which to turn on', () => {
    const off = { ...on, claudeEnabled: false, codexEnabled: false };
    expect(trackerToolsProvider('agy', off)).toBeNull();
    expect(trackerToolsBlocked(off, 'Laptop')).toBe('Ask needs Claude or Codex, whose agents get the tracker tools; turn one on in Settings on Laptop.');
    expect(trackerToolsBlocked(on, 'Laptop')).toBe('');
  });
});
