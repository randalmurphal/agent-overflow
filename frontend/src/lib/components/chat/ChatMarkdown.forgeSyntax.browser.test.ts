import { render, waitFor } from '@testing-library/svelte';
import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import katexCss from 'katex/dist/katex.min.css?inline';
import '../../../app.css';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import ChatMarkdown from './ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './markdown/forgeAttachmentContext';
import { markdownFindText } from './markdown/markdownFindText';
import { findTextSegments } from '../../utils/findHighlights';
import { foldFindText } from '../../utils/findMatch';
import type { ForgeAttachmentSource } from '../../utils/forgeAttachments';

// Forge syntax in real Chromium, as the review pane mounts a comment: the
// swatch paints the authored color, a GitLab paragraph's soft line break
// shows as a space, and find's projection reads the text the DOM shows.

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

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  document.body.innerHTML = '';
});

async function mounted(source: string, forge: ForgeAttachmentSource | null, review = true, streaming = false): Promise<HTMLElement> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
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

describe('color chips', () => {
  it.each([
    ['GitHub', GITHUB, '`#0969DA` `rgb(9, 105, 218)`', ['rgb(9, 105, 218)', 'rgb(9, 105, 218)']],
    ['GitLab', GITLAB, '`#F00` `RGB(0,255,0)` `HSLA(540,70%,50%,0.3)`', ['rgb(255, 0, 0)', 'rgb(0, 255, 0)', 'rgba(38, 217, 217, 0.3)']],
  ] as const)('paint the authored color beside the %s code', async (_name, forge, source, colors) => {
    const root = await mounted(`${source}\n\nlast line`, forge);
    const swatches = [...root.querySelectorAll<HTMLElement>('code > [data-streamdown-color-swatch]')];
    expect(swatches.map((swatch) => getComputedStyle(swatch).backgroundColor)).toEqual(colors);
    for (const swatch of swatches) {
      const box = swatch.getBoundingClientRect();
      expect(box.width).toBeGreaterThan(4);
      expect(box.height).toBeCloseTo(box.width, 0);
      // Beside the code's text, on its line.
      const code = swatch.parentElement!.getBoundingClientRect();
      expect(box.top).toBeGreaterThanOrEqual(code.top - 1);
      expect(box.bottom).toBeLessThanOrEqual(code.bottom + 1);
    }
  });
});

describe('GitLab inline diff', () => {
  // GitLab paints `.idiff` as a tinted highlight: no underline, no strike.
  it.each([false, true])('highlights an addition and a removal at normal contrast (streaming %s)', async (streaming) => {
    const root = await mounted('Diff {+ added +} and {- removed -} and ~~struck~~.\n\nlast line', GITLAB, true, streaming);
    const paragraph = root.querySelector('p')!;
    const [added, removed, struck] = [...paragraph.querySelectorAll('ins, del')];
    expect([added.textContent, removed.textContent, struck.textContent]).toEqual([' added ', ' removed ', 'struck']);
    const backgrounds = [added, removed].map((element) => getComputedStyle(element).backgroundColor);
    for (const element of [added, removed]) {
      const style = getComputedStyle(element);
      expect(style.textDecorationLine).toBe('none');
      expect(style.color).toBe(getComputedStyle(paragraph).color);
    }
    expect(backgrounds).not.toContain('rgba(0, 0, 0, 0)');
    expect(backgrounds[0]).not.toBe(backgrounds[1]);
    // A strikethrough still strikes.
    expect(getComputedStyle(struck).textDecorationLine).toBe('line-through');
  });
});

describe('display math in a paragraph', () => {
  // The test config stubs KaTeX's stylesheet for every other test.
  beforeAll(() => {
    const style = document.createElement('style');
    style.textContent = katexCss;
    document.head.appendChild(style);
    return () => style.remove();
  });
  const CAUCHY = String.raw`$$\left( \sum_{k=1}^n a_k b_k \right)^2 \leq \left( \sum_{k=1}^n a_k^2 \right) \left( \sum_{k=1}^n b_k^2 \right)$$`;
  const SOURCES = [`**Title**\\\n${CAUCHY}`, 'text $$a^2+b^2=c^2$$ text'];

  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('typesets $$…$$ on its own centered line on %s', async (_name, forge) => {
    for (const source of SOURCES) {
      document.body.innerHTML = '';
      const root = await mounted(`${source}\n\nlast line`, forge);
      const paragraph = root.querySelector('p')!;
      await waitFor(() => expect(paragraph.querySelector('.katex-display .katex-base')).not.toBeNull());
      // Phrasing content only, so an HTML parse would not split the paragraph.
      expect(paragraph.querySelector('div, pre')).toBeNull();
      // The text before the formula.
      const before = document.createRange();
      before.setStart(paragraph, 0);
      before.setEndBefore(paragraph.querySelector('[data-math-source]')!);
      const bases = [...paragraph.querySelectorAll<HTMLElement>('.katex-display .katex-base')].map((base) => base.getBoundingClientRect());
      const left = Math.min(...bases.map((box) => box.left));
      const right = Math.max(...bases.map((box) => box.right));
      expect(Math.min(...bases.map((box) => box.top))).toBeGreaterThanOrEqual(before.getBoundingClientRect().bottom);
      const column = paragraph.getBoundingClientRect();
      expect(left).toBeGreaterThan(column.left + 20);
      expect((left + right) / 2).toBeCloseTo((column.left + column.right) / 2, -1);
    }
  });

  it('keeps the streaming host phrasing content', async () => {
    const root = await mounted('text $$a^2+b^2=c^2$$ text last line', GITHUB, true, true);
    expect(root.querySelector('p')!.querySelector('div, pre')).toBeNull();
    expect(root.querySelector('p .math-inline')?.textContent).toBe('a^2+b^2=c^2');
  });
});

describe('soft line breaks', () => {
  const lines = (paragraph: Element) => {
    const range = document.createRange();
    range.selectNodeContents(paragraph);
    return new Set([...range.getClientRects()].map((rect) => Math.round(rect.top))).size;
  };

  it('show as a space in a GitLab paragraph, a <br> on GitHub, and a line break in agent chat', async () => {
    const source = 'alpha\nbeta\n\nlast line';
    expect(lines((await mounted(source, GITLAB)).querySelector('p')!)).toBe(1);
    expect(lines((await mounted(source, GITHUB)).querySelector('p')!)).toBe(2);
    expect(lines((await mounted(source, null, false)).querySelector('p')!)).toBe(2);
  });

  it('collapse a run of blanks in a review paragraph', async () => {
    const root = await mounted('alpha     beta\n\nlast line', GITLAB);
    const range = document.createRange();
    range.selectNodeContents(root.querySelector('p')!);
    const wide = range.getBoundingClientRect().width;
    const narrow = (await mounted('alpha beta\n\nlast line', GITLAB)).querySelector('p')!;
    range.selectNodeContents(narrow);
    expect(wide).toBeCloseTo(range.getBoundingClientRect().width, 0);
  });
});

describe("find's projection of forge syntax", () => {
  const SOURCES: [string, ForgeAttachmentSource][] = [
    ['Struck ~one~ and ~~two~~ and `#0969DA` and {+ literal +}\n\nlast line', GITHUB],
    [
      [
        'Diff {+ added +} and {- removed -} and ~one~ and `\\#FF0000` and `#F00`.',
        '>>>\nquoted one\n\nquoted two\n>>>',
        '- [~] skipped task\n- [ ]\n- [x] done task',
        '| Done | Task |\n| --- | --- |\n| [x] | cell task |',
        'Fruits\n: apple\n: orange',
        '```json:table\n{"items": [{"a": "11", "b": "**22**"}], "markdown": true, "caption": "Numbers"}\n```',
        'last line',
      ].join('\n\n'),
      GITLAB,
    ],
    ['---\ntitle: About\n---\n\nlast line', GITLAB],
  ];

  it.each(SOURCES)('counts what the comment shows: %s', async (source, forge) => {
    const root = await mounted(source, forge);
    const text = (run: string) => foldFindText(run).replace(/\s+/g, ' ').trim();
    const dom = findTextSegments(root).map(text).filter(Boolean);
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: forge }).runs.map(text).filter(Boolean);
    expect(projected).toEqual(dom);
  });
});
