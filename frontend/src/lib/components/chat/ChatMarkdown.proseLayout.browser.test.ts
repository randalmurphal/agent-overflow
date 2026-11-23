import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './markdown/forgeAttachmentContext';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Layout the platform decides, in real Chromium: where a task item's box
// sits, and the direction review prose lays its text out in.

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
const SURFACES = [
  ['GitHub', GITHUB],
  ['GitLab', GITLAB],
  ['agent chat', null],
] as const;

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  document.body.innerHTML = '';
});

async function mounted(source: string, forge: ForgeAttachmentSource | null, streaming = false): Promise<HTMLElement> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  const review = forge !== null;
  const { container } = render(ChatMarkdown, {
    target: host,
    props: review
      ? { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose', streaming }
      : { source, pathRefs: EMPTY_PATH_REFS, streaming },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
  });
  await waitFor(() => expect(container.querySelector('.markdown-body')?.textContent).toContain('last line'));
  return container.querySelector<HTMLElement>('.markdown-body')!;
}

/** The box of `node`'s first non-blank character. */
function firstCharRect(root: Element): DOMRect {
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    const at = node.textContent!.search(/\S/);
    if (at === -1) continue;
    const range = document.createRange();
    range.setStart(node, at);
    range.setEnd(node, at + 1);
    return range.getBoundingClientRect();
  }
  throw new Error(`no text in ${root.outerHTML}`);
}

const TASKS = [
  '- [ ] tight one',
  '- [x] tight two',
  '  - [ ] nested tight',
  '',
  'between',
  '',
  '1. [ ] ordered one',
  '2. [x] ordered two',
  '',
  'between',
  '',
  '* [ ] loose one',
  '',
  '* [x] loose two',
  '',
  '  second paragraph',
  '',
  '  - [ ] nested in loose',
  '',
  'between',
  '',
  '1. [ ] loose ordered one',
  '',
  '2. [x] loose ordered two',
  '',
  'last line',
].join('\n');

describe('a task item', () => {
  it.each(SURFACES.flatMap(([name, forge]) => [
    [name, 'settled', forge, false],
    [name, 'streaming', forge, true],
  ] as const))('puts its box beside its first line on %s, %s', async (_name, _state, forge, streaming) => {
    const root = await mounted(TASKS, forge, streaming);
    const items = [...root.querySelectorAll<HTMLElement>('li.md-task-list-item')];
    expect(items).toHaveLength(10);
    for (const item of items) {
      const box = item.querySelector(':scope > input[type="checkbox"]')!.getBoundingClientRect();
      const first = firstCharRect(item);
      // Same line: the box's middle lies within the first character's line.
      const middle = (box.top + box.bottom) / 2;
      expect(middle, item.textContent!).toBeGreaterThan(first.top);
      expect(middle, item.textContent!).toBeLessThan(first.bottom);
      expect(box.right, item.textContent!).toBeLessThanOrEqual(first.left);
    }
    // A loose item's later paragraph starts its own line.
    const loose = items.find((item) => item.textContent!.includes('loose two'))!;
    const paragraphs = loose.querySelectorAll(':scope > p');
    expect(paragraphs).toHaveLength(2);
    expect(paragraphs[1].getBoundingClientRect().top).toBeGreaterThanOrEqual(firstCharRect(paragraphs[0]).bottom);
    expect(firstCharRect(paragraphs[1]).left).toBeLessThan(loose.querySelector(':scope > input')!.getBoundingClientRect().right);
  });
});

const ARABIC = 'مرحبا بالعالم، هذا نص عربي!';
const MIXED = 'مرحبا hello world (test)!';
const ENGLISH_FIRST = 'Hello مرحبا world!';

/** Each character's left edge in `element`, from its container's left. */
function charLefts(element: Element, origin: number): number[] {
  const lefts: number[] = [];
  const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    for (let at = 0; at < node.textContent!.length; at++) {
      const range = document.createRange();
      range.setStart(node, at);
      range.setEnd(node, at + 1);
      const rect = range.getBoundingClientRect();
      if (rect.width > 0) lefts.push(Math.round(rect.left - origin));
    }
  }
  return lefts;
}

/** Where `element`'s content box ends on the right. */
function contentRight(element: Element): number {
  const style = getComputedStyle(element);
  return element.getBoundingClientRect().right - parseFloat(style.paddingRight) - parseFloat(style.borderRightWidth);
}

/** A `dir="auto"` element beside `element`, as a forge renders it: same
 * tag, same parent, the text alone, its direction from HTML. */
function forgeTwin(element: HTMLElement, text: string): HTMLElement {
  const twin = document.createElement(element.tagName);
  twin.setAttribute('dir', 'auto');
  // The twin's direction comes from `dir` alone, not from the rule under test.
  twin.style.unicodeBidi = 'isolate';
  twin.className = element.className;
  twin.textContent = text;
  element.after(twin);
  return twin;
}

describe('review prose text direction', () => {
  it.each([
    ['GitHub', GITHUB],
    ['GitLab', GITLAB],
  ] as const)('lays out each block by its own text on %s, as dir="auto" does', async (_name, forge) => {
    const source = [
      ARABIC,
      '',
      MIXED,
      '',
      ENGLISH_FIRST,
      '',
      `## ${ARABIC}`,
      '',
      `- ${MIXED}`,
      `- ${ENGLISH_FIRST}`,
      '',
      '| a |',
      '| --- |',
      `| ${ARABIC} |`,
      '',
      'last line',
    ].join('\n');
    const root = await mounted(source, forge);
    const blocks: [HTMLElement, string][] = [
      [root.querySelectorAll('p')[0], ARABIC],
      [root.querySelectorAll('p')[1], MIXED],
      [root.querySelectorAll('p')[2], ENGLISH_FIRST],
      [root.querySelector('h2')!, ARABIC],
      [root.querySelectorAll('li')[0], MIXED],
      [root.querySelectorAll('li')[1], ENGLISH_FIRST],
      [root.querySelector('tbody td')!, ARABIC],
    ];
    for (const [element, text] of blocks) {
      expect(element.textContent!.trim()).toBe(text);
      const twin = forgeTwin(element, text);
      const origin = element.getBoundingClientRect().left;
      expect(charLefts(element, origin), text).toEqual(charLefts(twin, twin.getBoundingClientRect().left));
      twin.remove();
    }
    // Right to left text ends at its block's right edge.
    for (const element of [root.querySelectorAll('p')[0], root.querySelector('h2')!, root.querySelectorAll('li')[0]]) {
      const span = document.createRange();
      span.selectNodeContents(element);
      const end = contentRight(element);
      expect(Math.abs(end - span.getBoundingClientRect().right), element.tagName).toBeLessThan(1);
    }
  });

  it('keeps an authored alignment and a centered wrapper', async () => {
    const source = [`<p align="left">${ARABIC}</p>`, '', '<div align="center">', '', ARABIC, '', '</div>', '', 'last line'].join('\n');
    const root = await mounted(source, GITHUB);
    const [left, centered] = [...root.querySelectorAll('p')];
    const range = document.createRange();
    range.selectNodeContents(left);
    expect(Math.abs(range.getBoundingClientRect().left - left.getBoundingClientRect().left)).toBeLessThan(1);
    range.selectNodeContents(centered);
    const text = range.getBoundingClientRect();
    const box = centered.getBoundingClientRect();
    expect(Math.abs(text.left - box.left - (box.right - text.right))).toBeLessThanOrEqual(1);
  });

  it('leaves agent chat left to right', async () => {
    const root = await mounted(`${ARABIC}\n\nlast line`, null);
    const paragraph = root.querySelector('p')!;
    const twin = document.createElement('p');
    twin.setAttribute('dir', 'ltr');
    twin.className = paragraph.className;
    twin.textContent = ARABIC;
    paragraph.after(twin);
    const origin = paragraph.getBoundingClientRect().left;
    expect(charLefts(paragraph, origin)).toEqual(charLefts(twin, twin.getBoundingClientRect().left));
  });
});

describe('a table column', () => {
  const TABLE = ['| plain | left | mid | right |', '| --- | :--- | :---: | ---: |', '| a | b | c | d |', '', 'last line'].join('\n');
  // An `align` attribute computes to Chromium's `-webkit-` keyword of the same side.
  const textAlignOf = (cell: Element) => getComputedStyle(cell).textAlign.replace('-webkit-', '');
  const HTML_TABLE = '<table><tr><th>h</th><td align="right">r</td><td align="center">c</td><td>p</td></tr></table>\n\nlast line';

  it.each(SURFACES)('takes its authored alignment on %s, and starts at the left without one', async (_name, forge) => {
    const root = await mounted(TABLE, forge);
    const aligned = (selector: string) => [...root.querySelectorAll(selector)].map(textAlignOf);
    expect(aligned('th')).toEqual(['left', 'left', 'center', 'right']);
    expect(aligned('td')).toEqual(['left', 'left', 'center', 'right']);
  });

  it('takes an HTML cell alignment on a forge surface', async () => {
    const root = await mounted(HTML_TABLE, GITLAB);
    expect([...root.querySelectorAll('th, td')].map(textAlignOf)).toEqual(['left', 'right', 'center', 'left']);
  });
});
