import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { describe, expect, it } from 'vitest';

const markedModuleUrl = pathToFileURL(
  resolve(
    import.meta.dirname,
    './parser/index.ts',
  ),
).href;

// The workload runs in a bare `node --expose-gc` subprocess, because the
// measurement is a whole-heap delta and vitest's own heap is not quiet. Node
// strips the types itself, but it resolves specifiers the way Node does: the
// parser's internal imports are extensionless (`moduleResolution: bundler`),
// so the subprocess registers a resolve hook that retries with `.ts`, then
// with `/index.ts`. Nothing else in the app loads this tree outside Vite.
const workload = `
import nodeModule from 'node:module';
nodeModule.registerHooks({
  resolve(specifier, context, next) {
    if (specifier.startsWith('.') && !/\\.[a-z]+$/.test(specifier)) {
      try { return next(specifier + '.ts', context); }
      catch { return next(specifier + '/index.ts', context); }
    }
    return next(specifier, context);
  },
});

// Dynamic, because a static import is hoisted above the hook registration.
const {
  createMaterializedProvenAppend,
  createParseBlocksCache,
  createProvenAppend,
  parseBlocks,
  updateParseBlockStringMaterialization,
} = await import(${JSON.stringify(markedModuleUrl)});

for (let index = 0; index < 4; index++) globalThis.gc();
const before = process.memoryUsage().heapUsed;
const cache = createParseBlocksCache();
let source = '';
for (let index = 0; index < 800; index++) {
  const marker = index === 0 ? 'AO_PARSE_BLOCK_RETENTION ' : '';
  const delta = marker +
    'Paragraph ' + String(index).padStart(4, '0') +
    ' keeps enough parser content to produce a non-trivial backing string while streaming.\\n\\n';
  const append = process.env.AO_CHAIN_PARSE_SOURCE === '1'
    ? createProvenAppend(source, delta)
    : createMaterializedProvenAppend(source, [delta]);
  source = append.next;
  parseBlocks(source, [], cache, append);
  if (process.env.AO_DETACH_PARSE_BLOCKS === '1') {
    updateParseBlockStringMaterialization(cache, true);
  }
}
globalThis.__aoParseBlockRetention = { cache, source };
for (let index = 0; index < 4; index++) globalThis.gc();
process.stdout.write(String(process.memoryUsage().heapUsed - before));
`;

function retainedHeapBytes(detach: boolean, chainedSource = false): number {
  const output = execFileSync(
    process.execPath,
    ['--expose-gc', '--input-type=module', '--eval', workload],
    {
      encoding: 'utf8',
      env: {
        ...process.env,
        AO_DETACH_PARSE_BLOCKS: detach ? '1' : '0',
        AO_CHAIN_PARSE_SOURCE: chainedSource ? '1' : '0',
      },
    },
  );
  const bytes = Number(output);
  if (!Number.isFinite(bytes)) {
    throw new Error(`parse-block retention subprocess returned ${JSON.stringify(output)}`);
  }
  return bytes;
}

describe('parse block backing retention', () => {
  it('does not retain one full parser checkpoint per completed block', () => {
    const retained = retainedHeapBytes(false);
    const detached = retainedHeapBytes(true);

    // On V8, marked's block raws are SlicedStrings backed by the full input.
    // The control retains about 35 MB for this 73 KB document. Detaching the
    // raws keeps only the final source and the 800 small block strings.
    expect(retained).toBeGreaterThan(20 * 1024 * 1024);
    expect(detached).toBeLessThan(3 * 1024 * 1024);
    expect(detached).toBeLessThan(retained / 8);
  });

  it('keeps completed blocks detached when the parser source is an append rope', () => {
    const retained = retainedHeapBytes(true, true);

    // The streaming boundary splitter normally keeps marked on bounded source
    // pieces. This stronger direct-parse case proves that even a growing
    // append rope cannot recreate the historical-checkpoint retention once
    // completed block raws are detached.
    expect(retained).toBeLessThan(3 * 1024 * 1024);
  });
});
