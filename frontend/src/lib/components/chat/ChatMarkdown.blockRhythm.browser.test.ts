import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';
import { resetCodeSpanCacheForTest } from './markdown/codeSpanCache';
import { resetCodeWrapStateForTest } from './markdown/codeWrapState';
import { __resetStreamdownCodeHostForTest } from './markdown/StreamdownCodeHost.svelte';

// Real-Chromium geometry for the markdown block rhythm: every pair of
// adjacent blocks is separated by the same `--markdown-block-gap` (one body
// em, 13px), whatever the two block types are, and a heading adds half a gap
// above itself only. Headings, lists and the other blocks own their margins
// in different places (app.css element rules, theme classes, the paragraph
// adjacency class), so a drift in any one of them shows up here as one
// transition measuring differently from the rest.

const BODY_PX = 13;
const HEADING_TOP_PX = BODY_PX * 1.5;

const SOURCE = [
  'Intro paragraph.',
  '**Label**',
  '- one\n- two\n  - nested\n- three',
  'Paragraph after list.',
  '## Heading two',
  'Paragraph after heading.',
  '### Heading three',
  '- list after heading',
  '```ts\nconst a = 1;\n```',
  '| A | B |\n| --- | --- |\n| 1 | 2 |',
  '> quoted',
  '---',
  '1. first\n2. second',
  '> [!NOTE]\n> An alert.',
  '$$\nx^2\n$$',
  'Closing paragraph.',
].join('\n\n');

beforeEach(() => {
  resetCodeSpanCacheForTest();
  resetCodeWrapStateForTest();
  __resetStreamdownCodeHostForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
  document.body.innerHTML = '';
});

function markdownBody(): HTMLElement {
  const body = document.createElement('div');
  body.className = 'markdown-body';
  body.style.width = '600px';
  document.body.appendChild(body);
  return body;
}

function gap(first: Element, second: Element): number {
  return second.getBoundingClientRect().top - first.getBoundingClientRect().bottom;
}

/** Every adjacent top-level block pair must sit exactly one gap apart,
 *  except that a heading sits one and a half gaps below its predecessor. */
function expectUniformRhythm(blocks: Element[]): void {
  expect(blocks.length).toBeGreaterThan(10);
  const measured = blocks.slice(1).map((block, index) => ({
    pair: `${blocks[index].tagName} -> ${block.tagName}`,
    gap: Math.round(gap(blocks[index], block) * 10) / 10,
  }));
  const expected = blocks.slice(1).map((block, index) => ({
    pair: `${blocks[index].tagName} -> ${block.tagName}`,
    gap: /^H[1-6]$/.test(block.tagName) ? HEADING_TOP_PX : BODY_PX,
  }));
  expect(measured).toEqual(expected);
}

describe('markdown block rhythm', () => {
  it('spaces every settled block transition by one block gap', async () => {
    const body = markdownBody();
    render(ChatMarkdown, { target: body, props: { source: SOURCE, pathRefs: [] } });
    const root = await waitFor(() => {
      const committed = body.querySelector('.md-committed');
      expect(committed?.querySelector('.math-display')).toBeTruthy();
      expect(committed?.querySelector('pre')).toBeTruthy();
      return committed!;
    });
    expectUniformRhythm([...root.children]);
  });

  it('keeps a nested list tight against its item', async () => {
    const body = markdownBody();
    render(ChatMarkdown, { target: body, props: { source: SOURCE, pathRefs: [] } });
    const nested = await waitFor(() => {
      const found = body.querySelector('li > ul');
      expect(found).not.toBeNull();
      return found!;
    });
    expect(getComputedStyle(nested).marginTop).toBe('4px');
    expect(getComputedStyle(nested).marginBottom).toBe('4px');
  });

  it('leaves the code block interior outside the block gap', async () => {
    const body = markdownBody();
    render(ChatMarkdown, { target: body, props: { source: SOURCE, pathRefs: [] } });
    // `.markdown-body pre` also matches the pre inside the code block wrapper,
    // where its margin is interior space, not the gap between blocks.
    const pre = await waitFor(() => {
      const found = body.querySelector('.streamdown-code-host pre');
      expect(found).not.toBeNull();
      return found!;
    });
    expect(getComputedStyle(pre).marginTop).toBe('8px');
    expect(getComputedStyle(pre).marginBottom).toBe('8px');
  });

  // The component render path in production is the streaming volatile tail:
  // each block type rendered there must sit exactly where it will sit once
  // committed, or the settle re-split moves it.
  it.each([
    ['paragraph', 'Tail paragraph being typed', 'P', BODY_PX],
    ['list', '- one\n- two', 'UL', BODY_PX],
    ['ordered list', '1. one\n2. two', 'OL', BODY_PX],
    ['heading', '## Heading', 'H2', HEADING_TOP_PX],
    ['code block', '```ts\nconst a = 1;', 'DIV', BODY_PX],
    ['table', '| A | B |\n| --- | --- |\n| 1 | 2 |', 'DIV', BODY_PX],
    ['blockquote', '> quoted', 'BLOCKQUOTE', BODY_PX],
    ['math', '$$\nx^2', 'DIV', BODY_PX],
  ])('places a streaming %s tail one gap below the committed prefix', async (_, tail, tag, expected) => {
    const body = markdownBody();
    render(ChatMarkdown, {
      target: body,
      props: { source: `Intro paragraph.\n\n${tail}`, pathRefs: [], streaming: true },
    });
    const [committed, first] = await waitFor(() => {
      const last = body.querySelector('.md-committed')?.lastElementChild;
      const head = body.querySelector('.md-volatile')?.firstElementChild;
      expect(last?.tagName).toBe('P');
      expect(head?.tagName).toBe(tag);
      return [last!, head!];
    });
    expect(gap(committed, first)).toBeCloseTo(expected, 1);
  });

  it.each([
    ['list', '- one\n- two\n\nTail paragraph being typed'],
    ['heading', '## Heading\n\nTail paragraph being typed'],
  ])('keeps the %s -> paragraph gap across the streaming seam', async (_, source) => {
    const body = markdownBody();
    render(ChatMarkdown, { target: body, props: { source, pathRefs: [], streaming: true } });
    const [committed, tail] = await waitFor(() => {
      const last = body.querySelector('.md-committed')?.lastElementChild;
      const first = body.querySelector('.md-volatile')?.firstElementChild;
      expect(first?.tagName).toBe('P');
      return [last!, first!];
    });
    expect(gap(committed, tail)).toBeCloseTo(BODY_PX, 1);
  });
});
