import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { SRC_ROOT, scannedSources } from './sourceScan';

describe('scannedSources', () => {
  it('reads the files a glob loaded, by absolute path', () => {
    const scanned = scannedSources({ '/src/lib/a.ts': 'a', '/src/lib/b.svelte': 'b' });
    expect(scanned.files(/\.ts$/)).toEqual([join(SRC_ROOT, 'lib', 'a.ts')]);
    expect(scanned.text(join(SRC_ROOT, 'lib', 'b.svelte'))).toBe('b');
    expect(() => scanned.files(/\.css$/)).toThrow('no source files found; the rules would pass vacuously');
  });

  it('rejects a glob that loaded a test, an operator driver or the test tree', () => {
    for (const key of ['/src/lib/a.test.ts', '/src/lib/a.spec.js', '/src/lib/a.manual.ts', '/src/test/helpers/x.ts']) {
      expect(() => scannedSources({ [key]: '' })).toThrow(`${key}: a tripwire's glob must exclude tests, operator drivers and /src/test/`);
    }
    expect(() => scannedSources({ './a.ts': '' })).toThrow("./a.ts: a tripwire's glob names files from /src/");
  });
});
