import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  createParseBlocksCache,
  createProvenAppend,
  parseBlocks,
  updateParseBlockStringMaterialization,
  type ParseBlocksCache,
  type ParseBlocksLexPath,
} from './index';
import { StreamingBoundarySplitter } from './boundary';
import { wordUnitOffsets } from './freezeReplayHarness';

interface ScenarioEmit {
  lines: string[];
}

interface ScenarioRepeat {
  steps: Array<{ emit?: ScenarioEmit }>;
}

interface ActiveStreamScenario {
  turns: Array<{
    steps: Array<{ repeat?: ScenarioRepeat }>;
  }>;
}

interface LexMetrics {
  calls: number;
  codeUnits: number;
  maxInput: number;
  byPath: Record<ParseBlocksLexPath, { calls: number; codeUnits: number; maxInput: number }>;
  resultPaths: Partial<Record<ParseBlocksCache['lastPath'], number>>;
}

function createMetrics(): LexMetrics {
  return {
    calls: 0,
    codeUnits: 0,
    maxInput: 0,
    byPath: {
      full: { calls: 0, codeUnits: 0, maxInput: 0 },
      'append-tail': { calls: 0, codeUnits: 0, maxInput: 0 },
      'list-descent': { calls: 0, codeUnits: 0, maxInput: 0 },
      'table-descent': { calls: 0, codeUnits: 0, maxInput: 0 },
    },
    resultPaths: {},
  };
}

function parseObserved(
  markdown: string,
  cache: ParseBlocksCache,
  metrics: LexMetrics,
  append?: ReturnType<typeof createProvenAppend>,
): void {
  parseBlocks(markdown, [], cache, append);
  metrics.resultPaths[cache.lastPath] = (metrics.resultPaths[cache.lastPath] ?? 0) + 1;
}

function observingCache(metrics: LexMetrics): ParseBlocksCache {
  return createParseBlocksCache((path, inputLength) => {
    metrics.calls++;
    metrics.codeUnits += inputLength;
    metrics.maxInput = Math.max(metrics.maxInput, inputLength);
    const pathMetrics = metrics.byPath[path];
    pathMetrics.calls++;
    pathMetrics.codeUnits += inputLength;
    pathMetrics.maxInput = Math.max(pathMetrics.maxInput, inputLength);
  });
}

function activeStreamText(iterations: number): string {
  const scenarioPath = resolve(
    process.cwd(),
    '../internal/harness/scenario/library/bench-active-stream.json',
  );
  const scenario = JSON.parse(
    readFileSync(scenarioPath, 'utf8'),
  ) as ActiveStreamScenario;
  const repeat = scenario.turns[0]?.steps.find((step) => step.repeat)?.repeat;
  const lines = repeat?.steps.find((step) => step.emit)?.emit?.lines;
  if (!lines) throw new Error('active stream scenario has no repeating emit lines');

  let text = '';
  for (let iteration = 0; iteration < iterations; iteration++) {
    for (const line of lines) {
      const wire = JSON.parse(
        line
          .replaceAll('${ITER}', String(iteration))
          .replaceAll('${TURN}', '1')
          .replaceAll('${CWD}', '/tmp/ao-workload'),
      ) as { data?: { delta?: { text?: string } } };
      text += wire.data?.delta?.text ?? '';
    }
  }
  return text;
}

// Deliberately NOT behind AO_PERF_CONTRACT: every bound here counts WORK,
// not time. Code units fed to marked, the largest single input, and which
// fast path each step took are pure functions of the corpus and the cache
// logic, identical on an idle laptop and under a soak rig. Gating them
// would drop real coverage to buy nothing. Wall-clock contracts (the
// relative-cost assertions in incrementalLex.test.ts) are the gated kind.
describe('parseBlocks active-pane workload', () => {
  it('keeps marked input bounded to the block that can still change', () => {
    const text = activeStreamText(200);
    const metrics = createMetrics();
    const splitter = new StreamingBoundarySplitter();
    const prefixCache = observingCache(metrics);
    let tailCache: ParseBlocksCache | undefined;
    let previousSource = '';
    let previousPrefix: string | undefined;
    let previousTail: string | undefined;

    for (const offset of wordUnitOffsets(text)) {
      const delta = text.slice(previousSource.length, offset);
      const append = createProvenAppend(previousSource, delta);
      const source = append.next;
      const { prefix, tail } = splitter.split(source, append);

      if (prefix && prefix !== previousPrefix) {
        parseObserved(prefix, prefixCache, metrics);
        updateParseBlockStringMaterialization(prefixCache, true);
      }
      previousPrefix = prefix;

      if (tail || !prefix) {
        tailCache ??= observingCache(metrics);
        if (tail !== previousTail) {
          parseObserved(tail, tailCache, metrics, splitter.tailAppend);
        }
      } else {
        tailCache = undefined;
      }
      previousTail = tail;
      previousSource = source;
    }

    expect(metrics.codeUnits).toBeLessThan(430_000);
    expect(metrics.maxInput).toBeLessThan(400);
    expect(metrics.resultPaths['paragraph-append']).toBeGreaterThan(2_000);
    expect(metrics.resultPaths['line-block-append']).toBeGreaterThan(500);
    expect(metrics.resultPaths['list-line-append']).toBeGreaterThan(500);
    expect(metrics.resultPaths['table-line-append']).toBeGreaterThan(500);
    expect(metrics.resultPaths['fence-append']).toBeGreaterThan(500);
  });
});
