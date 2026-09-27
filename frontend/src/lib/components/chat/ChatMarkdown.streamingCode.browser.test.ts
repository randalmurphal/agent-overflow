import { render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { createProvenAppend } from '../../markdown';
import ChatMarkdown from './ChatMarkdown.svelte';
import { __codeSpanCacheStatsForTest, getCachedBlockSpans, resetCodeSpanCacheForTest } from './markdown/codeSpanCache';
import { __resetStreamdownCodeHostForTest } from './markdown/StreamdownCodeHost.svelte';

// A streaming code block in the live DOM. The DOM work of each streamed delta
// and each highlight response must not grow with the block, and the block's
// span requests must not grow the span cache.

beforeEach(() => {
  resetCodeSpanCacheForTest();
  __resetStreamdownCodeHostForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
});

// Colors each line's first word, so a line's colors depend only on its text.
function keywordSpans(source: string) {
  return {
    lang: 'ts',
    lines: source.split('\n').map((line) => {
      const word = line.indexOf(' ');
      return word > 0 ? { r: [word, 1] } : {};
    }),
    truncated: false,
  };
}

// Highlight requests the test answers one at a time.
function gatedHighlights() {
  const pending: Array<() => void> = [];
  setBindingMock('HighlightCode', (request: { source: string }) =>
    new Promise((resolve) => {
      pending.push(() => resolve(keywordSpans(request.source)));
    }));
  return {
    async answer(): Promise<void> {
      await waitFor(() => expect(pending).toHaveLength(1));
      pending.shift()!();
    },
  };
}

function codeElement(root: Element): HTMLElement {
  const code = root.querySelector<HTMLElement>('[data-code-source] code');
  if (!code) throw new Error('code block did not mount');
  return code;
}

// Every node under the code element, in document order, with its text.
function snapshot(code: HTMLElement): Array<{ node: Node; data: string | null }> {
  const walker = document.createTreeWalker(code, NodeFilter.SHOW_ALL);
  const nodes: Array<{ node: Node; data: string | null }> = [];
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    nodes.push({ node, data: node instanceof CharacterData ? node.data : null });
  }
  return nodes;
}

// Index of the source line holding the first node that changed since
// `before`. A '\n' text node joins lines; no segment contains one.
function firstChangedLine(before: ReturnType<typeof snapshot>, code: HTMLElement): number {
  const after = snapshot(code);
  let line = 0;
  for (let i = 0; i < before.length; i++) {
    if (after[i]?.node !== before[i].node || after[i].data !== before[i].data) return line;
    if (before[i].data === '\n') line += 1;
  }
  return after.length > before.length ? line : Number.POSITIVE_INFINITY;
}

const DELTAS = [
  '\nconst added',
  ' = 1;',
  '\nlet a = 1;\nlet b = 2;',
  ' // tail',
  '\n\nconst gap = 0;',
  'x',
];

interface StreamWork {
  deltaMutations: number[];
  responseMutations: number[];
}

async function streamInto(lineCount: number): Promise<StreamWork> {
  const highlights = gatedHighlights();
  const body = Array.from({ length: lineCount }, (_, i) => `const value_${i} = ${i};`).join('\n');
  let source = '```ts\n' + body;
  const view = render(ChatMarkdown, { props: { source, streaming: true, pathRefs: [] } });
  const code = codeElement(view.container);
  const keywords = (): number => code.querySelectorAll('.syntax-keyword').length;
  await highlights.answer();
  await waitFor(() => expect(keywords()).toBe(lineCount));

  const records: MutationRecord[] = [];
  const observer = new MutationObserver((batch) => records.push(...batch));
  observer.observe(code, { childList: true, characterData: true, subtree: true });
  const takeCount = (): number => {
    records.push(...observer.takeRecords());
    return records.splice(0).length;
  };
  const initial = snapshot(code);
  const work: StreamWork = { deltaMutations: [], responseMutations: [] };
  try {
    for (const delta of DELTAS) {
      const linesBefore = code.textContent!.split('\n').length;
      let before = snapshot(code);
      const append = createProvenAppend(source, delta);
      source = append.next;
      await view.rerender({ source, sourceAppend: append, streaming: true, pathRefs: [] });
      flushSync();
      expect(code.textContent).toBe(source.slice('```ts\n'.length));
      work.deltaMutations.push(takeCount());
      // Only the last line the delta extends, or whose stale colors it
      // drops, changes above the lines it adds.
      expect(firstChangedLine(before, code)).toBeGreaterThanOrEqual(linesBefore - 1);

      before = snapshot(code);
      await highlights.answer();
      const colored = keywordSpans(source.slice('```ts\n'.length)).lines.filter((line) => 'r' in line);
      await waitFor(() => expect(keywords()).toBe(colored.length));
      work.responseMutations.push(takeCount());
      expect(firstChangedLine(before, code)).toBeGreaterThanOrEqual(linesBefore - 1);
    }
  } finally {
    observer.disconnect();
  }

  // Every node of the lines above the original last line is still the node
  // the first render created, attached where it was.
  const current = snapshot(code);
  let line = 0;
  for (let i = 0; line < lineCount - 1; i++) {
    expect(current[i].node).toBe(initial[i].node);
    expect(current[i].node.isConnected).toBe(true);
    if (initial[i].data === '\n') line += 1;
  }
  view.unmount();
  return work;
}

describe('streaming code block', () => {
  it('does the same DOM work per delta and per highlight response at any length', async () => {
    const short = await streamInto(200);
    const long = await streamInto(4000);
    expect(long).toEqual(short);
  });

  it('keeps one span cache entry while it streams and completes from it', async () => {
    const rpc = setBindingMock('HighlightCode', async (request: { source: string }) =>
      keywordSpans(request.source));
    let source = '```ts\nconst line_0 = 0;';
    const view = render(ChatMarkdown, { props: { source, streaming: true, pathRefs: [] } });
    await waitFor(() => expect(rpc).toHaveBeenCalledTimes(1));
    for (let i = 1; i <= 40; i++) {
      const append = createProvenAppend(source, `\nconst line_${i} = ${i};`);
      source = append.next;
      await view.rerender({ source, sourceAppend: append, streaming: true, pathRefs: [] });
      await waitFor(() => expect(rpc).toHaveBeenCalledTimes(i + 1));
    }
    const code = codeElement(view.container);
    await waitFor(() => expect(code.querySelectorAll('.syntax-keyword')).toHaveLength(41));
    expect(__codeSpanCacheStatsForTest().entries).toBe(1);

    // The completed block renders from that entry without another request.
    await view.rerender({ source: `${source}\n\`\`\``, streaming: false, pathRefs: [] });
    await waitFor(() => {
      expect(view.container.querySelector('[data-static-code-copy]')).not.toBeNull();
    });
    expect(view.container.querySelectorAll('.syntax-keyword')).toHaveLength(41);
    expect(rpc).toHaveBeenCalledTimes(41);
    view.unmount();
  });

  // The tail keys its blocks by index, so the next fence streams in the host
  // that streamed the block before it.
  it('keeps a completed block\'s spans while the next block streams in its host', async () => {
    const pending: Array<() => void> = [];
    setBindingMock('HighlightCode', (request: { source: string }) =>
      new Promise((resolve) => {
        pending.push(() => resolve(keywordSpans(request.source)));
      }));
    const answerAll = async (): Promise<void> => {
      await waitFor(() => expect(pending.length).toBeGreaterThan(0));
      for (const answer of pending.splice(0)) answer();
    };
    const first = 'const a = 1;\nconst b = 2;';
    let source = '```ts\n' + first;
    const view = render(ChatMarkdown, { props: { source, streaming: true, pathRefs: [] } });
    await waitFor(() => expect(pending).toHaveLength(1));

    // The first block completes while its last request is in flight.
    source += '\n```\n\n```ts\nlet x';
    await view.rerender({ source, streaming: true, pathRefs: [] });
    await waitFor(() => expect(view.container.querySelectorAll('[data-code-source]')).toHaveLength(2));
    await answerAll();
    for (const delta of [' = 1;', '\nlet y = 2;', '\nlet z = 3;']) {
      const append = createProvenAppend(source, delta);
      source = append.next;
      await view.rerender({ source, sourceAppend: append, streaming: true, pathRefs: [] });
      await answerAll();
    }
    await waitFor(() => {
      expect(view.container.querySelectorAll('[data-code-source]')[1]?.textContent).toContain('let z = 3;');
    });
    expect(getCachedBlockSpans('ts', first)).not.toBeNull();
    view.unmount();
  });
});
