import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../../app.css';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import ChatMarkdown from '../ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../../utils/pathLinkify';
import { countFindMatches, findNeedle, foldFindText } from '../../../utils/findMatch';
import { findTextSegments } from '../../../utils/findHighlights';
import { markdownFindText } from './markdownFindText';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './forgeAttachmentContext';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { resetCodeSpanCacheForTest } from './codeSpanCache';
import { __resetStreamdownCodeHostForTest } from './StreamdownCodeHost.svelte';

// markdownFindText against the DOM a settled review comment renders
// (ChatMarkdown with embedded HTML, as ReviewThreadComments mounts it): for
// every word of the source and of the rendered text, the count over the
// projection is the count the highlighter finds in the DOM. Mermaid,
// math and forge attachment chips are neither counted nor painted; a
// <details> the reader toggles counts as they left it.

const HEX = '0123456789abcdef0123456789abcdef';
const FORGE: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};

const CORPUS = [
  'Plain words, and a sentence.',
  'Use **bold**, _em_, ~~gone~~ and `code_span()` here.',
  'A [link text](https://example.com/target) and <https://example.com/auto>.',
  '- one\n- two **2**\n  - nested\n- three\n\n1. first\n2. second',
  '- [ ] open task\n- [x] done task',
  '```ts\nconst needle = 1;\n```',
  '```suggestion\nreplacement line\n```',
  '> quoted **text**\n\nafter',
  '| Col A | Col B |\n| --- | --- |\n| a1 | `b1` |',
  '# Title\n\n## Sub title\n\nbody',
  '> [!NOTE]\n> An alert body.',
  'Escapes: \\*not em\\* and \\`tick\\`.',
  'Image ![alt words](https://example.com/x.png) after.',
  'line one  \nline two',
  '<details><summary>Summary words</summary>\n\nHidden body words\n\n</details>\n\nafter details',
  '<details open><summary>Open summary</summary>\n\nShown body words\n\n</details>',
  'Inline <b>bold html</b> and <code>tag code</code> and <span>stray span</span>.',
  '<table><tr><td>cell one</td><td>cell two</td></tr></table>',
  '<!-- a comment --> visible after comment',
  'Footnote ref[^1].\n\n[^1]: the note body',
  'Text with &amp; entity &lt;tag&gt; and &copy;.',
  '---\n\nafter rule',
  '```mermaid\ngraph TD; Start-->Finish\n```\n\nafter the diagram',
  '$$\nx^2 + yy\n$$\n\ninline $a+bb$ math words',
  `See ![shot alt](/uploads/${HEX}/shot.png) and [report.pdf](/uploads/${HEX}/report.pdf) here.`,
  `<img src="/uploads/${HEX}/embedded.png" alt="embedded alt"> after the embed`,
  '<table><tr><td><details><summary>cell sum</summary>inner cell words</details></td></tr></table>',
];

const TOGGLED = CORPUS.filter((source) => source.includes('<details'));

beforeEach(() => {
  resetCodeSpanCacheForTest();
  __resetStreamdownCodeHostForTest();
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  document.body.innerHTML = '';
});

function count(runs: readonly string[], query: string): number {
  const needle = findNeedle(query);
  return runs.reduce((total, run) => total + countFindMatches(foldFindText(run), needle), 0);
}

async function mounted(source: string): Promise<Element> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  const { container } = render(ChatMarkdown, {
    target: host,
    props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => FORGE]]),
  });
  await waitFor(() => expect(container.querySelector('.markdown-body')?.textContent?.trim()).toBeTruthy());
  // Let deferred islands (diagrams, formulas, attachments) settle.
  await new Promise((resolve) => setTimeout(resolve, 50));
  return container.querySelector('.markdown-body')!;
}

function mismatches(source: string, dom: readonly string[], projected: readonly string[]) {
  const queries = new Set<string>();
  for (const text of [source, ...dom]) for (const word of text.split(/\s+/)) if (word) queries.add(word);
  for (const run of dom) if (run.trim()) queries.add(run.trim());
  // A run of whitespace is layout (template spacing beside an island), not
  // text a reader can see or type.
  return [...queries].filter((query) => !/\s\s/.test(query))
    .map((query) => ({ query, dom: count(dom, query), projected: count(projected, query) }))
    .filter((row) => row.dom !== row.projected);
}

describe('markdownFindText', () => {
  it.each(CORPUS)('counts what the rendered comment shows: %s', async (source) => {
    const root = await mounted(source);
    const dom = findTextSegments(root);
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: FORGE });
    expect(projected.details).toEqual([...root.querySelectorAll('details')].map((details) => details.open));
    expect(mismatches(source, dom, projected.runs), JSON.stringify({ dom, projected })).toEqual([]);
    // A closed <details> body, an attachment chip, a diagram or a formula
    // is in neither.
    for (const hidden of ['hidden body words', 'inner cell words', 'shot.png', 'embedded', 'start-->finish', 'yy', 'bb']) {
      expect(dom.join('\n'), hidden).not.toContain(hidden);
    }
  });

  it('an attachment still loading is neither counted nor painted', async () => {
    setBindingMock('FetchForgeAttachment', () => new Promise(() => {}));
    const source = `Before ![pending alt](/uploads/${'f'.repeat(32)}/pending.png) after.`;
    const root = await mounted(source);
    expect(root.querySelector('[data-forge-attachment-loading]')).not.toBeNull();
    const dom = findTextSegments(root);
    expect(dom.join('\n')).not.toContain('pending');
    expect(mismatches(source, dom, markdownFindText(source, { embeddedHtml: true, forgeSource: FORGE }).runs)).toEqual([]);
  });

  it.each(TOGGLED)('counts a <details> as the reader left it: %s', async (source) => {
    const root = await mounted(source);
    for (const details of root.querySelectorAll('details')) details.open = !details.open;
    await new Promise((resolve) => setTimeout(resolve, 0));
    const open = [...root.querySelectorAll('details')].map((details) => details.open);
    const dom = findTextSegments(root);
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: FORGE }, open);
    expect(mismatches(source, dom, projected.runs), JSON.stringify({ dom, projected })).toEqual([]);
  });
});
