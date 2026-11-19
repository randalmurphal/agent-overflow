import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../../app.css';
import ChatMarkdown from '../ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../../utils/pathLinkify';
import { countFindMatches, findNeedle } from '../../../utils/findMatch';
import { findTextSegments } from '../../../utils/findHighlights';
import { markdownFindText } from './markdownFindText';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './forgeAttachmentContext';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';

// A forge body's same-document links in real Chromium: a jump stays in the
// body it was clicked in and moves only that body's scroll container; and
// find counts what the document-level constructs show.

const GITHUB: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};
const GITLAB: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};

const filler = (label: string) => Array.from({ length: 30 }, (_, index) => `${label} filler line ${index}.`).join('\n\n');
const BODY = (label: string) => [
  '[[_TOC_]]',
  '',
  `[Jump](#target) and [nowhere](#no-such-heading) and a note[^n].`,
  '',
  filler(label),
  '',
  '## Target',
  '',
  `${label} target text.`,
  '',
  filler(`${label} after`),
  '',
  '[^n]: The note.',
].join('\n');

beforeEach(() => {
  document.body.innerHTML = '';
});

/** A body in a section scroller, inside an outer scroller, as the review pane nests them. */
async function mountBody(outer: HTMLElement, source: string, forge: ForgeAttachmentSource): Promise<{ scroller: HTMLElement; root: Element }> {
  const scroller = document.createElement('div');
  scroller.style.cssText = 'height: 200px; overflow-y: auto; width: 500px;';
  outer.appendChild(scroller);
  const { container } = render(ChatMarkdown, {
    target: scroller,
    props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  const root = await waitFor(() => {
    const found = container.querySelector('.markdown-body');
    expect(found?.querySelector('[data-md-anchor="target"]')).not.toBeNull();
    return found!;
  });
  return { scroller, root };
}

function outerScroller(): HTMLElement {
  const outer = document.createElement('div');
  outer.style.cssText = 'height: 300px; overflow-y: auto;';
  const spacer = document.createElement('div');
  spacer.style.height = '50px';
  outer.appendChild(spacer);
  document.body.appendChild(outer);
  return outer;
}

const top = (node: Element, scroller: Element) => node.getBoundingClientRect().top - scroller.getBoundingClientRect().top;

describe('same-document links', () => {
  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('jump within the %s body clicked, moving only its scroller', async (_name, forge) => {
    const outer = outerScroller();
    const first = await mountBody(outer, BODY('First'), forge);
    const second = await mountBody(outer, BODY('Second'), forge);
    const tail = document.createElement('div');
    tail.style.height = '600px';
    outer.appendChild(tail);
    outer.scrollTop = 120;
    const hash = location.hash;

    const link = second.root.querySelector<HTMLAnchorElement>('p a[data-md-fragment][href="#target"]')!;
    link.click();
    const target = second.root.querySelector('[data-md-anchor="target"]')!;
    expect(Math.abs(top(target, second.scroller))).toBeLessThan(1);
    expect(second.scroller.scrollTop).toBeGreaterThan(0);
    expect(first.scroller.scrollTop).toBe(0);
    expect(outer.scrollTop).toBe(120);
    expect(location.hash).toBe(hash);

    // A link to nothing goes nowhere, and does not navigate either.
    const before = second.scroller.scrollTop;
    second.root.querySelector<HTMLAnchorElement>('a[href="#no-such-heading"]')!.click();
    expect(second.scroller.scrollTop).toBe(before);
    expect(location.hash).toBe(hash);

    // The footnote's back-reference returns to its reference.
    second.root.querySelector<HTMLAnchorElement>('.md-footnotes a[data-md-fragment]')!.click();
    expect(Math.abs(top(second.root.querySelector('[data-md-anchor="fnref:1"]')!, second.scroller))).toBeLessThan(1);
    expect(first.scroller.scrollTop).toBe(0);
  });

  it("follows GitLab's table of contents", async () => {
    const outer = outerScroller();
    const { scroller, root } = await mountBody(outer, BODY('Only'), GITLAB);
    root.querySelector<HTMLAnchorElement>('ul a[href="#target"]')!.click();
    expect(Math.abs(top(root.querySelector('[data-md-anchor="target"]')!, scroller))).toBeLessThan(1);
  });
});

function count(runs: readonly string[], query: string): number {
  const needle = findNeedle(query);
  return runs.reduce((total, run) => total + countFindMatches(run, needle), 0);
}

const FIND_SOURCES = [
  'Use [ref] and [text][Ref].\n\n[ref]: https://example.invalid/ref "Title"\n\nAfter.',
  'One[^b] two[^a] [^missing].\n\n[^a]: Alpha note.\n[^b]: Beta note\nlazy line.\n[^unused]: Unused words.',
  '[[_TOC_]]\n\n# Heading one\n\n## Heading two\n\n[TOC]\n\nInline [toc] text.',
  '# Sample\n\n<a name="custom"></a><a id="ided"></a>\n\n[jump](#sample) and [custom](#custom)',
];

describe('find over document-level constructs', () => {
  it.each(FIND_SOURCES.flatMap((source) => [[source, GITHUB], [source, GITLAB]] as const))('counts what the body shows: %s', async (source, forge) => {
    const host = document.createElement('div');
    host.style.width = '600px';
    document.body.appendChild(host);
    const { container } = render(ChatMarkdown, {
      target: host,
      props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
      context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
    });
    await waitFor(() => expect(container.querySelector('.markdown-body')?.textContent?.trim()).toBeTruthy());
    await new Promise((resolve) => setTimeout(resolve, 50));
    const dom = findTextSegments(container.querySelector('.markdown-body')!);
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: forge }).runs;
    const queries = new Set<string>();
    for (const text of [source, ...dom]) for (const word of text.split(/\s+/)) if (word) queries.add(word);
    const mismatches = [...queries]
      .map((query) => ({ query, dom: count(dom, query), projected: count(projected, query) }))
      .filter((row) => row.dom !== row.projected);
    expect(mismatches, JSON.stringify({ dom, projected })).toEqual([]);
  });
});
