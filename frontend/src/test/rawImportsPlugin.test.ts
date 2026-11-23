import { describe, expect, it } from 'vitest';
import { rawImportsPlugin } from './config/rawImportsPlugin';

type Context = { resolve(source: string, importer: string): Promise<{ id: string } | null>; addWatchFile(file: string): void };

async function watched(code: string, id = '/repo/frontend/src/a.test.ts'): Promise<string[]> {
  const files: string[] = [];
  const context: Context = {
    resolve: async (source) => (source.includes('missing') ? null : { id: `/repo/${source.replace(/^(\.\.\/)+/, '')}` }),
    addWatchFile: (file) => files.push(file),
  };
  const hook = rawImportsPlugin().transform as (this: Context, code: string, id: string) => Promise<null>;
  await hook.call(context, code, id);
  return files;
}

describe('rawImportsPlugin', () => {
  it('watches the file behind a raw import of a non-module file, static or dynamic', async () => {
    expect(await watched(`import a from '../../internal/x.go?raw'; const b = import('../index.html?raw');`)).toEqual([
      '/repo/internal/x.go',
      '/repo/index.html',
    ]);
  });

  it('leaves raw imports of scripts and stylesheets, which the walk already follows, and plain imports', async () => {
    expect(await watched(`import a from './a.svelte?raw'; import b from './b.css?raw'; import c from './c.json';`)).toEqual([]);
  });

  it('reads only script modules', async () => {
    expect(await watched(`.drawer { color: red }`, '/repo/frontend/src/a.css')).toEqual([]);
  });

  it('fails a raw import that does not resolve', async () => {
    await expect(watched(`import a from './missing.go?raw';`)).rejects.toThrow('imports ./missing.go?raw, which does not resolve');
  });
});
