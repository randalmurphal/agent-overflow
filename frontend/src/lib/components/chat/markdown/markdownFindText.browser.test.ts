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

// A GitHub PR's: the one source whose extension turns text the parser
// would show (a bare attachment URL, an autolink) into an embed.
const GITHUB_FORGE: ForgeAttachmentSource = {
  pr: { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 4 },
  backend: 'home',
  webBase: 'https://github.com/acme/widget/pull/4',
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
  // Every place the renderers drop, replace or reorder what the source says.
  '| Head | Other |\n| --- | --- |\n| top | x |\n| merged ^ | y |\n| below | z |',
  '| Head | Other |\n| --- | --- |\n| wide cell ||\n| a | b |',
  '[titled link](https://example.com "the title words") and [bad link](javascript:alert(1)) end',
  '[a file](src/a.ts), [an anchor](#frag) and [mail](mailto:x@example.com).',
  'Visit https://example.com/bare and www.example.org today.',
  'H~2~O and x^2^ power, ~5~10 approx',
  ': term words : detail words\n: second term : second detail',
  'Cites [1] [ref two] here.',
  'Open http://localhost:5173/app now.',
  '<Note kind="x">mdx child words</Note> after',
  'Copy &nbsp;non&#8209;breaking &#x41; and \\backslash',
  '[center]\naligned needle words\n[/center]\n\nafter the block',
  '[right]\nright needle\n\n- listed needle\n[/right]\n[center]\nnext needle\n[/center]',
  '[center]\nunclosed needle\n\n- listed needle\n\nlast needle',
  '[center]\n<Aside>\nFirst needle paragraph.\n\nSecond needle paragraph.\n</Aside>\n[/center]',
  '[center]\nneedle\n[/right]\n\nstill centered needle\n\n[/center]\n[/center]',
  '[typed](streamdown:incomplete-link) and [label][ref]\n\n[ref]: https://example.com/ref',
  '- item\n\n  loose paragraph\n\n- next',
  '| Name | Count |\n| --- | --- |\n| apples | 3 |\n| --- | --- |\n| total footer | 3 |',
  // A claimed video's fallback children, a <details> among them, are dropped.
  `<div><video src="/uploads/${HEX}/clip.mp4">needle<details><summary>fallback</summary>inner</details></video></div>\n\n<details open><summary>sum</summary>\n\nneedle BODY\n\n</details>\n\nneedle KEEP\n\nneedle NEXT`,
];

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
  return runs.reduce((total, run) => total + countFindMatches(foldFindText(run), needle), 0);
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
  function random(seed: number): () => number {
    let state = seed >>> 0;
    return () => {
      state = (state + 0x6d2b79f5) >>> 0;
      let t = state;
      t = Math.imul(t ^ (t >>> 15), t | 1);
      t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }

  function comment(seed: number): string {
    const next = random(seed);
    const pick = <T,>(items: readonly T[]): T => items[Math.floor(next() * items.length)];
    const words = () => Array.from({ length: 1 + Math.floor(next() * 4) }, () => pick(['needle', 'word', 'Needle', 'x', 'needleneedle'])).join(' ');
    const open = () => (next() < 0.5 ? ' open' : '');
    const block = (depth: number): string => {
      const kinds = depth > 2 ? 7 : 12;
      switch (Math.floor(next() * kinds)) {
        // Prose, with forge attachments as markdown: chips, neither counted nor painted.
        case 0: return next() < 0.5 ? words() : `${words()} [${words()}](/uploads/${HEX}/r.pdf) ![${words()}](/uploads/${HEX}/a.png) ${words()}`;
        case 1: return `> [!${pick(['NOTE', 'WARNING', 'TIP'])}]\n> ${words()}`;
        case 2: return `| ${words()} | b |\n| --- | --- |\n| ${words()} | \`${words()}\` |`;
        case 3: return '```\n' + words() + '\n```';
        case 4: return `<div><video src="/uploads/${HEX}/clip.mp4">${words()}<details><summary>${words()}</summary>${words()}</details></video></div>`;
        case 5: return `<div><script>${words()}()</script><foo>${words()}</foo><!-- ${words()} --> ${words()}</div>`;
        case 6: return `<div><video src="https://example.com/v.mp4">${words()}</video> <img src="/uploads/${HEX}/a.png" alt="${words()}"> ${words()}</div>`;
        case 7: return `<details${open()}><summary>${words()}</summary>\n\n${block(depth + 1)}\n\n</details>`;
        case 8: return `<details${open()}><summary>${words()}</summary>\n\n${block(depth + 1)}\n\n</details>\n<details${open()}><summary>${words()}</summary>\n\n${words()}\n\n</details>`;
        case 9: return `<table><tr><td><details${open()}><summary>${words()}</summary>${words()}</details></td><td>${words()}</td></tr></table>`;
        // Closed, unclosed (aligned to the end), or closed by the other kind (text).
        case 10: {
          const align = pick(['center', 'right']);
          const closer = pick([`\n[/${align}]`, '', `\n[/${align === 'center' ? 'right' : 'center'}]`]);
          return `[${align}]\n${words()}\n\n${block(depth + 1)}${closer}`;
        }
        default: return `- ${words()}\n- ${block(depth + 1).replace(/\n/g, ' ')}`;
      }
    };
    return Array.from({ length: 2 + Math.floor(next() * 4) }, () => block(0)).join('\n\n');
  }

  const SEEDS = Array.from({ length: 60 }, (_, i) => i + 1);

  it('generates alignment blocks of both kinds, closed and unclosed', () => {
    const sources = SEEDS.map(comment);
    expect(sources.join('\n')).toContain('[/center]');
    expect(sources.join('\n')).toContain('[/right]');
    expect(sources.some((source) => /^\[(center|right)\]$/m.test(source) && !/^\[\/(center|right)\]$/m.test(source))).toBe(true);
  });

  it.each(SEEDS)('seed %i', async (seed) => {
    const source = comment(seed);
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
