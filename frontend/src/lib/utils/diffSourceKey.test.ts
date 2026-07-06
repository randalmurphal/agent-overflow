import { describe, expect, it } from 'vitest';
import { diffSourceKey } from './diffSourceKey';

describe('diffSourceKey', () => {
  it('pins the persisted FNV-1a source-key format', () => {
    const patch = [
      'diff --git a/app.ts b/app.ts',
      '--- a/app.ts',
      '+++ b/app.ts',
      '@@ -1 +1 @@',
      '-old',
      '+new',
    ].join('\n');

    expect(diffSourceKey(patch)).toBe('fnv1a:e9c7e391:76');
  });
});
