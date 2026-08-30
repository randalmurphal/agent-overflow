import { describe, expect, it } from 'vitest';
import {
  createIncrementalLexCache,
  createParseBlocksCache,
  createProvenAppend,
  incrementalLex,
  parseBlocks,
  parseIncompleteMarkdown,
  type IncrementalLexCache,
  type ParseBlocksCache,
  type ProvenAppend,
  type StreamdownToken,
} from './index';
import { StreamingBoundarySplitter } from './boundary';
import { expectedStreamingFenceTexts } from '../../test/helpers/streamingFenceOracle';

function workloadSection(iteration: number): string {
  return [
    `\n\n### Working set ${iteration}\n\n`,
    `The active pane keeps **streamed Markdown**, \`inline code\`, and [a link](https://example.test/active/${iteration}) readable. `,
    `Unicode remains intact: café, 東京, 🧪, and iteration ${iteration}.\n\n`,
    '- The parser carries state across wire chunks.\n- The reveal queue stays bounded.\n- The spring follows the live edge.\n\n',
    `| Iteration | Parser | Scroll |\n| ---: | :--- | :--- |\n| ${iteration} | active | following |\n\n`,
    '```ts\n',
    `const sample${iteration} = { pane: 1, active: true };\n`,
    `console.log(sample${iteration});\n\`\`\`\n\n`,
    `> Visible progress marker ${iteration}. The next section continues the same ordinary long turn.`,
  ].join('');
}

const WORKLOAD = Array.from({ length: 40 }, (_, index) => workloadSection(index)).join('');

function codeTexts(tokens: readonly StreamdownToken[]): string[] {
  const result: string[] = [];
  const visit = (nested: readonly StreamdownToken[]): void => {
    for (const token of nested) {
      if (token.type === 'code') result.push(token.text ?? '');
      const children = (token as StreamdownToken & {
        tokens?: StreamdownToken[];
      }).tokens;
      if (Array.isArray(children)) visit(children);
    }
  };
  visit(tokens);
  return result;
}

class IncrementalStreamdownModel {
  private readonly blockCache: ParseBlocksCache = createParseBlocksCache();
  private readonly lexCaches: IncrementalLexCache[] = [];

  render(
    content: string,
    complete: boolean,
    contentAppend?: ProvenAppend,
  ): string[] {
    const blocks = parseBlocks(content, [], this.blockCache, contentAppend);
    this.lexCaches.length = blocks.length;
    const result: string[] = [];
    for (let index = 0; index < blocks.length; index++) {
      const cache = this.lexCaches[index] ??= createIncrementalLexCache();
      const tokens = incrementalLex(
        blocks[index],
        [],
        cache,
        complete ? null : parseIncompleteMarkdown,
        index === blocks.length - 1
          ? this.blockCache.lastBlockAppend
          : undefined,
      );
      result.push(...codeTexts(tokens));
    }
    return result;
  }
}

class StreamingMarkdownPipelineModel {
  private readonly splitter = new StreamingBoundarySplitter();
  private committed: IncrementalStreamdownModel | undefined;
  private volatile: IncrementalStreamdownModel | undefined;

  render(source: string, append?: ProvenAppend): string[] {
    const split = this.splitter.split(source, append);
    const result: string[] = [];
    if (split.prefix) {
      this.committed ??= new IncrementalStreamdownModel();
      result.push(...this.committed.render(split.prefix, true));
    }
    if (split.tail || !split.prefix) {
      this.volatile ??= new IncrementalStreamdownModel();
      result.push(...this.volatile.render(split.tail, false, this.splitter.tailAppend));
    } else {
      this.volatile = undefined;
    }
    return result;
  }
}

function prefixEnds(source: string, sizes: readonly number[]): number[] {
  const ends: number[] = [];
  let offset = 0;
  let index = 0;
  while (offset < source.length) {
    offset = Math.min(source.length, offset + sizes[index % sizes.length]);
    ends.push(offset);
    index++;
  }
  return ends;
}

function seededSizes(seed: number, count: number): number[] {
  const sizes: number[] = [];
  let state = seed >>> 0;
  for (let index = 0; index < count; index++) {
    state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0;
    sizes.push(1 + (state % 96));
  }
  return sizes;
}

describe('streaming Markdown pipeline differential', () => {
  it.each([
    ['one code unit', [1]],
    ['reveal-sized', [7, 13, 21]],
    ['wire-sized', [64, 128]],
    ['jittered', seededSizes(0xa511_e9b3, 4096)],
  ] as const)('keeps completed fences closed under %s source advances', (_name, sizes) => {
    const pipeline = new StreamingMarkdownPipelineModel();
    let previous = '';
    for (const end of prefixEnds(WORKLOAD, sizes)) {
      const source = WORKLOAD.slice(0, end);
      const append = createProvenAppend(previous, source.slice(previous.length));
      const actual = pipeline.render(source, append);
      const expected = expectedStreamingFenceTexts(source);
      if (expected.hasOpenFence && actual.length > 0) {
        actual[actual.length - 1] = actual.at(-1)!.trimEnd();
      }
      expect(
        actual,
        `source length ${source.length}, tail ${JSON.stringify(source.slice(-120))}`,
      ).toEqual(expected.texts);
      previous = source;
    }
  });
});
