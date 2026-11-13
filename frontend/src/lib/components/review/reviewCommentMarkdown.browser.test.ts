// In real Chromium: a comment body is one Markdown document with each
// placed suggestion rendered where its fence is. A <details> around a
// suggestion still collapses it, a footnote across one keeps its
// definition (its reference opens the note), identical suggestions each take their own fence, a
// suggestion-looking fence in a list that is not a suggestion stays code,
// and find counts what the pane shows.

import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import { createRawSnippet } from 'svelte';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ReviewCommentMarkdown from './ReviewCommentMarkdown.svelte';
import FootnotePopoverHost from '../chat/FootnotePopoverHost.svelte';
import { markdownFindText } from '../chat/markdown/markdownFindText';
import { resetCodeSpanCacheForTest } from '../chat/markdown/codeSpanCache';
import { __resetStreamdownCodeHostForTest } from '../chat/markdown/StreamdownCodeHost.svelte';
import { countFindMatches, findNeedle, foldFindText } from '../../utils/findMatch';
import { findTextSegments } from '../../utils/findHighlights';
import { isSuggestionMark, markSuggestionFences, type PlacedBlock } from '../../utils/suggestionMarks';

beforeEach(() => {
  resetCodeSpanCacheForTest();
  __resetStreamdownCodeHostForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  document.body.innerHTML = '';
});

const block = createRawSnippet((at: () => number) => ({
  render: () => `<div data-testid="block" data-at="${at()}" data-find-skip>proposed change ${at()}</div>`,
}));

async function mounted(body: string, blocks: (PlacedBlock | null)[]): Promise<HTMLElement> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  render(ReviewCommentMarkdown, { target: host, props: { body, blocks, block } });
  await waitFor(() => expect(host.querySelector('.markdown-body')?.textContent?.trim()).toBeTruthy());
  await new Promise((resolve) => setTimeout(resolve, 50));
  return host;
}

function shown(host: HTMLElement): number[] {
  return [...host.querySelectorAll<HTMLElement>('[data-testid="block"]')].map((each) => Number(each.dataset.at));
}

function precedes(a: Node, b: Node): boolean {
  return Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
}

function textNode(host: HTMLElement, text: string): Node {
  const walker = document.createTreeWalker(host, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) if (node.textContent?.includes(text)) return node;
  throw new Error(`no text ${text}`);
}

describe('a comment body with suggestions', () => {
  it('collapses a suggestion inside a closed <details> with the section', async () => {
    const body = '<details><summary>Proposal</summary>\n\nInside words.\n\n```suggestion\nx\n```\n\n</details>\n\nAfter words.';
    const host = await mounted(body, [{ startLine: 4, endLine: 6, lines: ['x'] }]);
    const details = host.querySelector('details')!;
    const diff = host.querySelector<HTMLElement>('[data-testid="block"]')!;
    expect(details.contains(diff)).toBe(true);
    expect(diff.checkVisibility()).toBe(false);
    host.querySelector<HTMLElement>('summary')!.click();
    await waitFor(() => expect(diff.checkVisibility()).toBe(true));
    expect(host.querySelectorAll('.markdown-body')).toHaveLength(1);
  });

  it('keeps a footnote whose reference and definition sit across a suggestion', async () => {
    const body = 'See the note[^n].\n\n```suggestion\nx\n```\n\n[^n]: The note body.';
    render(FootnotePopoverHost);
    const host = await mounted(body, [{ startLine: 2, endLine: 4, lines: ['x'] }]);
    expect(host.textContent).not.toContain('[^n]');
    const ref = host.querySelector<HTMLElement>('[data-footnote-label="n"]')!;
    ref.click();
    await waitFor(() => expect(document.querySelector('[data-footnote-popover]')?.textContent).toContain('The note body.'));
    expect(precedes(textNode(host, 'See the note'), host.querySelector('[data-testid="block"]')!)).toBe(true);
  });

  it('renders two identical suggestions each at its own fence', async () => {
    const body = 'First.\n\n```suggestion\nsame\n```\n\nSecond.\n\n```suggestion\nsame\n```\n\nThird.';
    const host = await mounted(body, [{ startLine: 2, endLine: 4, lines: ['same'] }, { startLine: 8, endLine: 10, lines: ['same'] }]);
    expect(shown(host)).toEqual([0, 1]);
    const [one, two] = host.querySelectorAll('[data-testid="block"]');
    expect(precedes(textNode(host, 'First.'), one) && precedes(one, textNode(host, 'Second.'))).toBe(true);
    expect(precedes(textNode(host, 'Second.'), two) && precedes(two, textNode(host, 'Third.'))).toBe(true);
    expect(host.querySelector('pre')).toBeNull();
  });

  it('leaves a suggestion-looking fence in a list as code and renders the real one at its fence', async () => {
    const body = '- An item:\n\n  ```suggestion\n  x\n  ```\n\n```suggestion\nx\n```\n\nEnd.';
    const host = await mounted(body, [{ startLine: 6, endLine: 8, lines: ['x'] }]);
    expect(shown(host)).toEqual([0]);
    const listed = host.querySelector('li')!;
    expect(listed.querySelector('[data-testid="block"]')).toBeNull();
    expect(listed.textContent).toContain('x');
    const diff = host.querySelector('[data-testid="block"]')!;
    expect(precedes(listed, diff) && precedes(diff, textNode(host, 'End.'))).toBe(true);
  });

  it('keeps a claimed fence claimed when the same code is highlighted as code', async () => {
    let highlighted = 0;
    setBindingMock('HighlightCode', async (request: { source: string }) => {
      highlighted += 1;
      return { lang: 'suggestion', lines: request.source.split('\n').map(() => ({})), truncated: false };
    });
    const body = '- An item:\n\n  ```suggestion\n  x\n  ```\n\n```suggestion\nx\n```';
    const blocks = [{ startLine: 6, endLine: 8, lines: ['x'] }];
    await mounted(body, blocks);
    await waitFor(() => expect(highlighted).toBeGreaterThan(0));
    await new Promise((resolve) => setTimeout(resolve, 100));
    document.body.innerHTML = '';
    // The span cache is warm for this code now: a claimed block must not
    // serialize to static code.
    const host = await mounted(body, blocks);
    expect(shown(host)).toEqual([0]);
    expect(host.querySelectorAll('pre')).toHaveLength(1);
  });

  it('renders a block whose fence no longer holds its lines after the body, the fence as code', async () => {
    const body = 'Before.\n\n```suggestion\nedited\n```\n\nAfter.';
    const host = await mounted(body, [{ startLine: 2, endLine: 4, lines: ['x'] }]);
    const diff = host.querySelector('[data-testid="block"]')!;
    expect(host.querySelector('pre')?.textContent).toContain('edited');
    expect(precedes(textNode(host, 'After.'), diff)).toBe(true);
  });

  it('counts in find what the pane shows', async () => {
    const body = '<details open><summary>Sum needle</summary>\n\nneedle inside\n\n```suggestion\nneedle proposed\n```\n\n</details>\n\n- item\n\n  ```suggestion\n  needle listed\n  ```\n\n```suggestion\nneedle proposed\n```\n\nneedle[^n] end\n\n[^n]: needle note';
    const blocks = [{ startLine: 4, endLine: 6, lines: ['needle proposed'] }, { startLine: 16, endLine: 18, lines: ['needle proposed'] }];
    const host = await mounted(body, blocks);
    expect(shown(host)).toEqual([0, 1]);
    const dom = findTextSegments(host.querySelector('.markdown-body')!);
    const projected = markdownFindText(markSuggestionFences(body, blocks).source, { embeddedHtml: true, claimsCode: isSuggestionMark });
    const count = (runs: readonly string[], query: string) => runs.reduce((total, run) => total + countFindMatches(foldFindText(run), findNeedle(query)), 0);
    for (const query of ['needle', 'proposed', 'listed', 'inside', 'note', 'end', 'ao-suggestion']) {
      expect(count(projected.runs, query), query).toBe(count(dom, query));
    }
    expect(count(dom, 'needle')).toBe(4);
    expect(count(dom, 'proposed')).toBe(0);
    expect(projected.details).toEqual([true]);
  });
});
