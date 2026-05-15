import { describe, expect, it } from 'vitest';
import { pathBasename } from './pathDisplay';

describe('pathBasename', () => {
  it('returns the final path segment', () => {
    expect(pathBasename('/tmp/worktrees/feature-demo')).toBe('feature-demo');
  });

  it('ignores trailing separators', () => {
    expect(pathBasename('/tmp/worktrees/feature-demo///')).toBe('feature-demo');
  });

  it('handles Windows separators', () => {
    expect(pathBasename('C:\\worktrees\\feature-demo')).toBe('feature-demo');
  });

  it('returns an empty string for empty or root-only paths', () => {
    expect(pathBasename(undefined)).toBe('');
    expect(pathBasename('')).toBe('');
    expect(pathBasename('/')).toBe('');
  });
});
