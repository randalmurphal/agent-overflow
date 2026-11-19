import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it } from 'vitest';
import '../../../../app.css';
import { setBindingMock } from '../../../../test/mocks/bindings-app';
import ChatMarkdown from '../ChatMarkdown.svelte';
import { EMPTY_PATH_REFS } from '../../../utils/pathLinkify';
import { countFindMatches, findNeedle, foldFindText } from '../../../utils/findMatch';
import { findTextRuns, findTextSegments } from '../../../utils/findHighlights';
import { markdownFindText, SHOWS } from './markdownFindText';
import { EMBEDDED_HTML_EXTENSIONS, lex, parseBlocks } from '../../../markdown';
import { buildForgeAttachmentExtension } from '../../../utils/forgeAttachmentExtension';
import { FORGE_ATTACHMENT_SOURCE_CONTEXT } from './forgeAttachmentContext';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { resetCodeSpanCacheForTest } from './codeSpanCache';
import { __resetStreamdownCodeHostForTest } from './StreamdownCodeHost.svelte';
import { CORPUS, generatedMarkdown } from '../../../../test/fixtures/markdownFindCorpus';
import { assistantMarkdownSource } from '../threadFindText';

// markdownFindText against the DOM a settled review comment renders
// (ChatMarkdown with embedded HTML, as ReviewThreadComments mounts it): for
// every word of the source and of the rendered text, the count over the
// projection is the count the highlighter finds in the DOM. Mermaid,
// math and forge attachment chips are neither counted nor painted; a
// <details> the reader toggles counts as they left it.

const FORGE: ForgeAttachmentSource = {
  pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widget', number: 3 },
  backend: 'home',
  webBase: 'https://gitlab.com/group/widget/-/merge_requests/3',
};

// A GitHub PR's: the one source whose extension turns text the parser
// would show (a bare attachment URL, an autolink) into an embed.
const GITHUB_FORGE: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
};

const TOGGLED = CORPUS.filter((source) => source.includes('<details'));

// Every container a review comment can nest, around any markdown.
const indent = (text: string, by: string, first = by) => text.split('\n').map((line, at) => (at === 0 ? first : line ? by : '') + line).join('\n');
const CONTAINERS: Record<string, (inner: string) => string> = {
  details: (inner) => `<details><summary>outer summary</summary>\n\n${inner}\n\n</details>`,
  'open details': (inner) => `<details open><summary>open summary</summary>\n\n${inner}\n\n</details>`,
  blockquote: (inner) => indent(inner, '> '),
  'list item': (inner) => indent(inner, '  ', '- '),
  'table cell': (inner) => `| head |\n| --- |\n| ${inner.replace(/\n+/g, ' ')} |`,
  alert: (inner) => `> [!NOTE]\n${indent(inner, '> ')}`,
  'footnote definition': (inner) => `Ref[^n].\n\n${indent(inner, '    ', '[^n]: ')}`,
  'alignment block': (inner) => `[center]\n${inner}\n[/center]`,
  mdx: (inner) => `<Note kind="x">\n${inner}\n</Note>`,
  'html wrapper': (inner) => `<table><tr><td>\n\n${inner}\n\n</td></tr></table>`,
};
const INNER: Record<string, string> = {
  details: '<details><summary>inner summary</summary>\n\ninner needle words\n\n</details>',
  needle: 'plain needle words',
};
// A closed <details> after the nest, holding a needle of its own.
const AFTER = '<details><summary>after summary</summary>\n\nafter needle words\n\n</details>';
const NESTED: [string, string][] = [];
for (const [outer, wrapOuter] of Object.entries(CONTAINERS)) {
  for (const [inner, text] of Object.entries(INNER)) {
    NESTED.push([`${outer} > ${inner}`, `${wrapOuter(text)}\n\n${AFTER}`]);
    for (const [middle, wrapMiddle] of Object.entries(CONTAINERS)) {
      NESTED.push([`${outer} > ${middle} > ${inner}`, `${wrapOuter(wrapMiddle(text))}\n\n${AFTER}`]);
    }
  }
}

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
  return runs.reduce((total, run) => total + countFindMatches(run, needle), 0);
}

async function mounted(source: string, forge: ForgeAttachmentSource = FORGE): Promise<Element> {
  const host = document.createElement('div');
  host.style.width = '600px';
  document.body.appendChild(host);
  const { container } = render(ChatMarkdown, {
    target: host,
    props: { source, pathRefs: EMPTY_PATH_REFS, embeddedHtml: true, class: 'review-prose' },
    context: new Map([[FORGE_ATTACHMENT_SOURCE_CONTEXT, () => forge]]),
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

// Every token kind the projection has a rule for, as the corpus lexes.
function corpusKinds(): Set<string> {
  const extensions = [...buildForgeAttachmentExtension({ ...FORGE, embeddedHtml: true }), ...EMBEDDED_HTML_EXTENSIONS];
  const seen = new Set<string>();
  const visit = (tokens: readonly { type: string; tokens?: unknown[]; items?: unknown[] }[] | undefined) => {
    for (const token of tokens ?? []) {
      seen.add(token.type);
      visit(token.tokens as typeof tokens);
      visit(token.items as typeof tokens);
    }
  };
  for (const source of CORPUS) {
    for (const block of parseBlocks(source, extensions)) visit(lex(block, extensions) as never);
  }
  return seen;
}

describe('markdownFindText', () => {
  it('has a corpus case for every token kind the lexer produces', () => {
    const seen = corpusKinds();
    // lex() drops these at the top level; a block holds none.
    const dropped = new Set(['space', 'footnote']);
    expect(Object.keys(SHOWS).filter((kind) => !seen.has(kind) && !dropped.has(kind))).toEqual([]);
    expect([...seen].filter((kind) => !(kind in SHOWS))).toEqual([]);
  });

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

  it('a bare GitHub attachment URL renders as an embed, not as its URL, and is not counted', async () => {
    setBindingMock('FetchForgeAttachment', () => new Promise(() => {}));
    const url = 'https://github.com/user-attachments/assets/01234567-89ab-cdef-0123-456789abcdef';
    const source = `Before ${url} after.`;
    const root = await mounted(source, GITHUB_FORGE);
    const dom = findTextSegments(root);
    expect(dom.join('\n')).not.toContain('user-attachments');
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: GITHUB_FORGE }).runs;
    expect(count(projected, 'user-attachments')).toBe(0);
    expect(mismatches(source, dom, projected)).toEqual([]);
  });

  it.each([
    ['GitHub', GITHUB_FORGE],
    ['GitLab', FORGE],
  ] as const)('counts a soft line break and an entity reference as the %s comment shows them', async (_name, forge) => {
    const source = 'line one\nline two &amp; &copy; &#35;\n\n> quoted one\n> quoted two\n\n- item one\n  item more';
    const root = await mounted(source, forge);
    expect(root.querySelectorAll('br').length > 0).toBe(forge === GITHUB_FORGE);
    const dom = findTextSegments(root);
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: forge });
    expect(mismatches(source, dom, projected.runs), JSON.stringify({ dom, projected })).toEqual([]);
    const text = (run: string) => foldFindText(run).replace(/\s+/g, ' ').trim();
    expect(projected.runs.map(text).filter(Boolean)).toEqual(dom.map(text).filter(Boolean));
  });

  // A line break puts what follows on another line, so no match spans it.
  // Fixed counts, not the scanner and the projection agreeing.
  it.each([
    ['a GitHub soft break', 'alpha\nbeta', GITHUB_FORGE],
    ['a hard break on GitHub', 'alpha  \nbeta', GITHUB_FORGE],
    ['a hard break on GitLab', 'alpha  \nbeta', FORGE],
    ['a backslash break', 'alpha\\\nbeta', FORGE],
    ['an inline <br>', 'alpha<br>beta', FORGE],
    ['a <br> in an html block', '<p>alpha<br>beta</p>', FORGE],
    ['a <br> in an html region', '<div>\n<p>alpha<br>beta</p>\n\nafter\n\n</div>', FORGE],
  ] as const)('finds no match across %s', async (_name, source, forge) => {
    const root = await mounted(source, forge);
    expect(root.querySelector('br'), 'renders a line break').not.toBeNull();
    const dom = findTextSegments(root);
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: forge }).runs;
    for (const [query, want] of [['alphabeta', 0], ['alpha beta', 0], ['alpha', 1], ['beta', 1]] as const) {
      expect(count(dom, query), `${query} painted`).toBe(want);
      expect(count(projected, query), `${query} counted`).toBe(want);
    }
  });

  it('counts an html region island once, whatever the HTML around it spells', async () => {
    // `&#1;0&#1;` decodes to the control characters a text marker for
    // island 0 would be.
    const source = '<div>\n<p>&#1;0&#1;</p>\n\nneedle paragraph\n\n</div>';
    const root = await mounted(source);
    const dom = findTextSegments(root);
    const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: FORGE }).runs;
    expect(count(dom, 'needle'), 'painted').toBe(1);
    expect(count(projected, 'needle'), 'counted').toBe(1);
  });

  it.each(NESTED)('finds every <details> and counts what shows, as written and with each toggled: %s', async (_name, source) => {
    const root = await mounted(source);
    const elements = [...root.querySelectorAll('details')];
    const check = async (label: string) => {
      await new Promise((resolve) => setTimeout(resolve, 0));
      const open = elements.map((details) => details.open);
      const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: FORGE }, open);
      expect(projected.details.length, `${label}: details found`).toBe(elements.length);
      const dom = findTextSegments(root);
      expect(mismatches(source, dom, projected.runs), `${label}: ${JSON.stringify({ dom, projected })}`).toEqual([]);
    };
    expect(markdownFindText(source, { embeddedHtml: true, forgeSource: FORGE }).details).toEqual(elements.map((details) => details.open));
    await check('as written');
    for (const [at, details] of elements.entries()) {
      details.open = !details.open;
      await check(`details ${at} toggled`);
      details.open = !details.open;
    }
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

// Random review comments of real markdown and HTML, through the real
// projection and the real renderer: the runs the projection numbers are the
// rendered comment's, hidden <details> bodies included, as written and with
// each <details> toggled, and a toggle renumbers no run.
describe('markdownFindText over generated comments', () => {
  const SEEDS = Array.from({ length: 60 }, (_, i) => i + 1);

  it('generates alignment blocks of both kinds, closed and unclosed', () => {
    const sources = SEEDS.map(generatedMarkdown);
    expect(sources.join('\n')).toContain('[/center]');
    expect(sources.join('\n')).toContain('[/right]');
    expect(sources.some((source) => /^\[(center|right)\]$/m.test(source) && !/^\[\/(center|right)\]$/m.test(source))).toBe(true);
  });

  it.each(SEEDS)('seed %i', async (seed) => {
    const source = generatedMarkdown(seed);
    const root = await mounted(source);
    const elements = [...root.querySelectorAll('details')];
    const written = markdownFindText(source, { embeddedHtml: true, forgeSource: FORGE });
    expect(written.details, `${source}\ndetails`).toEqual(elements.map((details) => details.open));
    const check = async (label: string) => {
      await new Promise((resolve) => setTimeout(resolve, 0));
      const open = elements.map((details) => details.open);
      const projected = markdownFindText(source, { embeddedHtml: true, forgeSource: FORGE }, open);
      // Layout whitespace (template spacing beside an island, between
      // blocks) holds no match: a run of it is dropped and a span of it is
      // one space, on both sides.
      const text = (run: string) => foldFindText(run).replace(/\s+/g, ' ').trim();
      const fold = (runs: readonly { text: string; shown: boolean }[]) =>
        runs.map((run) => ({ text: text(run.text), shown: run.shown })).filter((run) => run.text !== '');
      // The rendered comment's runs, in walk order, hidden ones included.
      expect(fold(projected.all), `${label}\n${source}`).toEqual(fold(findTextRuns(root)));
      expect(projected.runs.map(text).filter(Boolean), `${label}: shown`).toEqual(findTextSegments(root).map(text).filter(Boolean));
      // A toggle renumbers nothing: the same runs, in the same places.
      expect(projected.all.map((run) => run.text), `${label}: numbering`).toEqual(written.all.map((run) => run.text));
    };
    await check('as written');
    for (const [at, details] of elements.entries()) {
      details.open = !details.open;
      await check(`details ${at} toggled`);
      details.open = !details.open;
    }
  });
});

// An assistant row's find text is its ChatMarkdown as AssistantMessage
// mounts it: no embedded HTML, no forge, through the raw-JSON fence
// (threadFindText.ts). The runs the projection gives are the rendered
// runs, so the thread find bar counts what it paints.
describe('markdownFindText under the chat options', () => {
  async function chatMounted(source: string): Promise<Element> {
    const host = document.createElement('div');
    host.style.width = '600px';
    document.body.appendChild(host);
    const { container } = render(ChatMarkdown, { target: host, props: { source, pathRefs: EMPTY_PATH_REFS } });
    // Chat drops embedded HTML, so a body can render no text at all.
    await waitFor(() => expect(container.querySelector('.markdown-body')).not.toBeNull());
    await new Promise((resolve) => setTimeout(resolve, 50));
    return container.querySelector('.markdown-body')!;
  }
  const text = (run: string) => foldFindText(run).replace(/\s+/g, ' ').trim();
  const sources = [
    ...CORPUS,
    ...Array.from({ length: 30 }, (_, i) => generatedMarkdown(i + 101)),
    '{"answer":"raw json needle","items":[1,2]}',
  ];

  it.each(sources)('gives the runs the chat render shows: %s', async (raw) => {
    const source = assistantMarkdownSource(raw);
    const root = await chatMounted(source);
    const projected = markdownFindText(source, { embeddedHtml: false }).runs;
    expect(projected.map(text).filter(Boolean), raw).toEqual(findTextSegments(root).map(text).filter(Boolean));
  });

  it('finds no match across a hard line break', async () => {
    const source = 'line one  \nline two\\\nline three';
    const root = await chatMounted(source);
    expect(root.querySelectorAll('br')).toHaveLength(2);
    const dom = findTextSegments(root);
    const projected = markdownFindText(source, { embeddedHtml: false }).runs;
    for (const [query, want] of [['oneline', 0], ['twoline', 0], ['line', 3], ['line one', 1]] as const) {
      expect(count(dom, query), `${query} painted`).toBe(want);
      expect(count(projected, query), `${query} counted`).toBe(want);
    }
  });

  it('counts none of a link target, a diagram, math or a blocked link tag, and the text markup joins', async () => {
    const source = '**foo**bar and [docs](https://example.com/x)suffix, $x+yy$ and [bad](javascript:alert(1))tail\n\n```mermaid\ngraph TD; Start-->Finish\n```';
    const root = await chatMounted(source);
    const dom = findTextSegments(root);
    const projected = markdownFindText(source, { embeddedHtml: false }).runs;
    for (const [query, want] of [['foobar', 1], ['docssuffix', 1], ['example', 0], ['yy', 0], ['blocked', 0], ['badtail', 0], ['finish', 0]] as const) {
      expect(count(dom, query), `${query} painted`).toBe(want);
      expect(count(projected, query), `${query} counted`).toBe(want);
    }
  });
});
