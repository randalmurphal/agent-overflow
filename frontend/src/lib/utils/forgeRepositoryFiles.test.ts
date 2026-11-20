import { describe, expect, it } from 'vitest';
import { createDocumentContextCache, createParseBlocksCache, documentContextOf, lex, parseBlocks, type ForgeFactReader, type Token } from '../markdown';
import { chatMarkdownExtensions, chatMarkdownForgeOptions } from '../components/chat/markdown/chatMarkdownOptions';
import type { ForgeAttachmentSource } from './forgeAttachments';
import { imageFactKey, linesFactKey, NO_REVISION, REVISION_FACT_KEY, type FactRevision, type ForgeFact, type ForgeFactState } from './forgeFacts';
import { forgeFactKeys } from './forgeReferenceExtension';
import { MAX_SNIPPET_LINES, permalinkCounters, permalinkOf, repositoryImageOf, snippetLanguage } from './forgeRepositoryFiles';
import { repositoryScanCounters } from './forgeRepositoryScan';
import { markdownFindText } from '../components/chat/markdown/markdownFindText';
import { parseRepositoryImageHref, REPOSITORY_IMAGE_HREF_PREFIX } from './repositoryImageHref';
import { repositoryFileCounters, repositoryFileOf, type RepositorySite } from './repositoryFiles';

// Which destinations name a file of the PR's repository (repositoryFiles.ts),
// what the parse claims for each fact state, the permalink snippet's token
// and split, and that the scan names every key a lex reads.

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
const GH_SITE: RepositorySite = { github: true, host: 'github.com', project: 'acme/widget' };
const GL_SITE: RepositorySite = { github: false, host: 'gitlab.com', project: 'group/widget' };
const HEAD = 'a'.repeat(40);
const BASE = 'b'.repeat(40);
const SHA = '0123456789abcdef0123456789abcdef01234567';
const OID = 'c'.repeat(40);
const REVISION: FactRevision = { head: HEAD, base: BASE, headRef: 'feature/x', baseRef: 'main' };
const GH = 'https://github.com/acme/widget';

describe('the repository file table', () => {
  it.each([
    ['docs/a.png', HEAD, 'docs/a.png'],
    ['./docs/a.png', HEAD, 'docs/a.png'],
    ['/docs/a.png', HEAD, 'docs/a.png'],
    ['docs/../a.png', HEAD, 'a.png'],
    ['docs/a%20b.png', HEAD, 'docs/a b.png'],
    ['docs/a%2520.png', HEAD, 'docs/a%20.png'],
    ['docs/a.png?raw=true#top', HEAD, 'docs/a.png'],
    ['../blob/main/docs/a.png', BASE, 'docs/a.png'],
    ['../raw/feature/x/docs/a.png', HEAD, 'docs/a.png'],
    [`../blob/${SHA.toUpperCase()}/a.png`, SHA, 'a.png'],
    [`${GH}/blob/main/a.png`, BASE, 'a.png'],
    ['https://github.com/Acme/Widget/raw/feature/x/a.png', HEAD, 'a.png'],
    [`${GH}/blob/feature/x/a.png?raw=true`, HEAD, 'a.png'],
    ['/acme/widget/blob/main/a.png', BASE, 'a.png'],
    ['https://raw.githubusercontent.com/acme/widget/main/a.png', BASE, 'a.png'],
    ['https://raw.githubusercontent.com/acme/widget/refs/heads/feature/x/a.png', HEAD, 'a.png'],
  ])('reads %s on GitHub', (href, commit, path) => {
    expect(repositoryFileOf(href, GH_SITE, () => REVISION)).toEqual({ commit, path });
  });

  it.each([
    '',
    '#frag',
    '?q=1',
    'docs/',
    '../a.png',
    'docs/../../a.png',
    '-x.png',
    'docs/a%zz.png',
    'mailto:a@b.c',
    'data:image/png;base64,AAAA',
    '../blob/other/a.png',
    '../blob/HEAD/a.png',
    'https://github.com/other/widget/blob/main/a.png',
    'https://example.com/acme/widget/blob/main/a.png',
    'https://user@github.com/acme/widget/blob/main/a.png',
    `${GH}/-/blob/main/a.png`,
    `${GH}/tree/main/a.png`,
    'https://raw.githubusercontent.com/other/widget/main/a.png',
  ])('reads nothing from %j on GitHub', (href) => {
    expect(repositoryFileOf(href, GH_SITE, () => REVISION)).toBeNull();
  });

  it.each([
    ['docs/a.png', HEAD, 'docs/a.png'],
    ['https://gitlab.com/group/widget/-/blob/main/a.png', BASE, 'a.png'],
    ['https://gitlab.com/group/widget/-/raw/HEAD/a.png', HEAD, 'a.png'],
    ['https://gitlab.com/group/widget/blob/feature/x/a.png', HEAD, 'a.png'],
    ['/group/widget/-/raw/main/a.png', BASE, 'a.png'],
  ])('reads %s on GitLab', (href, commit, path) => {
    expect(repositoryFileOf(href, GL_SITE, () => REVISION)).toEqual({ commit, path });
  });

  it.each(['../blob/main/a.png', 'https://raw.githubusercontent.com/group/widget/main/a.png', 'https://github.com/group/widget/blob/main/a.png'])(
    'reads nothing from %s on GitLab',
    (href) => expect(repositoryFileOf(href, GL_SITE, () => REVISION)).toBeNull(),
  );

  it('matches the longest branch name, slashes included', () => {
    const revision = { ...REVISION, headRef: 'feature/x', baseRef: 'feature' };
    expect(repositoryFileOf('../blob/feature/x/a.png', GH_SITE, () => revision)).toEqual({ commit: HEAD, path: 'a.png' });
    expect(repositoryFileOf('../blob/feature/y/a.png', GH_SITE, () => revision)).toEqual({ commit: BASE, path: 'y/a.png' });
  });

  it('names no file at the head before the frame has one', () => {
    expect(repositoryFileOf('docs/a.png', GH_SITE, () => NO_REVISION)).toBeNull();
    expect(repositoryFileOf(`../blob/${SHA}/a.png`, GH_SITE, () => NO_REVISION)).toEqual({ commit: SHA, path: 'a.png' });
  });

  it('asks for the revision only for a destination by branch or relative to the PR', () => {
    let asked = 0;
    const frame = () => (asked++, REVISION);
    for (const href of [`../blob/${SHA}/a.png`, 'https://example.com/a.png', '#x', 'mailto:a@b.c', `${GH}/blob/${SHA}/a.png`]) repositoryFileOf(href, GH_SITE, frame);
    expect(asked).toBe(0);
    repositoryFileOf('docs/a.png', GH_SITE, frame);
    repositoryFileOf('../blob/main/a.png', GH_SITE, frame);
    expect(asked).toBe(2);
  });

  it('plays a GitLab video or audio path as a player, not as an image', () => {
    expect(repositoryImageOf('docs/a.mp4', GL_SITE, () => REVISION)).toBeNull();
    expect(repositoryImageOf('docs/a.png', GL_SITE, () => REVISION)).toEqual({ commit: HEAD, path: 'docs/a.png' });
  });
});

describe('a permalink to lines', () => {
  const shapes = GH_SITE;
  it.each([
    [`${GH}/blob/${SHA}/src/x.ts#L3`, 3, 3],
    [`${GH}/blob/${SHA}/src/x.ts#L3-L7`, 3, 7],
    [`${GH}/blob/${SHA}/src/x.ts#L7-L3`, 3, 7],
    [`${GH}/blob/${SHA}/src/x.ts#L1-L${MAX_SNIPPET_LINES}`, 1, MAX_SNIPPET_LINES],
    [`${GH}/blob/${SHA}/src/x.ts?plain=1#L3`, 3, 3],
    [`${GH}/blob/${SHA}/src/x.ts?plain=1#L3-L5`, 3, 5],
  ])('reads %s', (href, start, end) => {
    expect(permalinkOf(href, shapes)).toEqual({ sha: SHA, path: 'src/x.ts', start, end, repo: 'widget', owner: 'acme' });
  });

  it.each([
    [`${GH}/blob/${SHA.slice(0, 7)}/src/x.ts#L3`, 'an abbreviated sha'],
    [`${GH}/blob/main/src/x.ts#L3`, 'a branch'],
    [`https://github.com/other/widget/blob/${SHA}/src/x.ts#L3`, 'another repository'],
    [`${GH}/blob/${SHA}/src/x.ts`, 'no fragment'],
    [`${GH}/blob/${SHA}/src/x.ts#L1-L${MAX_SNIPPET_LINES + 1}`, 'too many lines'],
    [`${GH}/blob/${SHA}/src/x.ts?plain=0#L3`, 'a query other than plain=1'],
    [`${GH}/blob/${SHA}/src/x.ts?plain=1&x=1#L3`, 'a second parameter'],
    [`${GH}/blob/${SHA}/src/x.ts?x=1&plain=1#L3`, 'plain=1 after another parameter'],
    [`${GH}/blob/${SHA}/src/x.ts?raw=1#L3`, 'another query'],
    [`${GH}/blob/${SHA}/src/x.ts#L3C2`, 'a column'],
    [`${GH}/blob/${SHA}/src/x.ts#L0`, 'line zero'],
    [`http://github.com/acme/widget/blob/${SHA}/src/x.ts#L3`, 'http'],
    [`${GH}/blob/${SHA}/../x.ts#L3`, 'a path that leaves the root'],
  ])('is no snippet for %s (%s)', (href) => {
    expect(permalinkOf(href, shapes)).toBeNull();
  });

  it('is never read on GitLab', () => {
    expect(permalinkOf(`https://gitlab.com/group/widget/-/blob/${SHA}/x.ts#L3`, GL_SITE)).toBeNull();
  });

  it.each([
    ['src/x.ts', 'ts'],
    ['Makefile', 'makefile'],
    ['build/Dockerfile', 'dockerfile'],
    ['README', ''],
    ['.gitignore', ''],
  ])('highlights %s as %j', (path, lang) => expect(snippetLanguage(path)).toBe(lang));
});

// ---------------------------------------------------------------------------
// The parse under facts
// ---------------------------------------------------------------------------

/** A reader over `facts` that answers the frame's revision as the store does, recording every key read. */
function reader(facts: Record<string, ForgeFactState>, rest: ForgeFactState = undefined, revision = REVISION): ForgeFactReader & { read: Set<string> } {
  const read = new Set<string>();
  const answer = (key: string): ForgeFactState =>
    key === REVISION_FACT_KEY ? { key, status: 'found', revision } : key in facts ? facts[key] : rest;
  return { read, get: (key) => (read.add(key), answer(key)), signature: (key) => JSON.stringify(answer(key)) ?? '' };
}

function lexUnder(markdown: string, source: ForgeAttachmentSource, facts: ForgeFactReader | null): Token[] {
  const extensions = chatMarkdownExtensions({ forge: chatMarkdownForgeOptions(source, true) });
  const cache = createParseBlocksCache();
  const blocks = parseBlocks(markdown, extensions, cache);
  const context = documentContextOf(cache, extensions, createDocumentContextCache(), source.pr.forge as 'github' | 'gitlab', facts);
  return blocks.flatMap((block, index) => lex(block, extensions, context.scope(index)) as unknown as Token[]);
}

type AnyToken = Token & { tokens?: Token[]; items?: Token[]; href: string; title?: string | null; sources?: Record<string, string> };

function each(tokens: readonly Token[], visit: (token: AnyToken) => void): void {
  for (const token of tokens as AnyToken[]) {
    visit(token);
    each(token.tokens ?? [], visit);
    each(token.items ?? [], visit);
  }
}

function images(tokens: readonly Token[]): AnyToken[] {
  const out: AnyToken[] = [];
  each(tokens, (token) => token.type === 'image' && out.push(token));
  return out;
}

const imageFound = (key: string, extra: Partial<ForgeFact> = {}): ForgeFact => ({ key, status: 'found', oid: OID, width: 640, height: 480, ...extra });
const KEY = imageFactKey(HEAD, 'docs/a.png');

describe('a repository image by its fact', () => {
  const written = [
    ['inline', '![shot](docs/a.png)'],
    ['by reference', '![shot][s]\n\n[s]: docs/a.png'],
    ['by collapsed reference', '![s][]\n\n[s]: ./docs/a.png'],
    ['as a branch URL', `![shot](${GH}/blob/feature/x/docs/a.png?raw=true)`],
    ['as a raw URL', '![shot](https://raw.githubusercontent.com/acme/widget/feature/x/docs/a.png)'],
    ['inside a link', '[![shot](docs/a.png)](https://example.com)'],
  ] as const;

  it.each(written)('claims it found with its oid and size (%s)', (_name, markdown) => {
    const [image] = images(lexUnder(markdown, GITHUB, reader({ [KEY]: imageFound(KEY) })));
    expect(image.href.startsWith(REPOSITORY_IMAGE_HREF_PREFIX)).toBe(true);
    const claim = parseRepositoryImageHref(image.href)!;
    expect(claim).toMatchObject({ state: 'found', backend: 'home', commit: HEAD, path: 'docs/a.png', oid: OID, width: 640, height: 480 });
    expect(claim.browserUrl).toBe(`${GH}/blob/${HEAD}/docs/a.png`);
    expect(image.title).toBe(claim.browserUrl);
  });

  it('carries a fluid fact\'s ratio as fluid, and a sized fact\'s size as pixels', () => {
    for (const [fact, fluid] of [[imageFound(KEY, { width: 16, height: 9, fluid: true }), true], [imageFound(KEY, { width: 16, height: 9 }), false]] as const) {
      const [image] = images(lexUnder('![shot](docs/a.png)', GITHUB, reader({ [KEY]: fact })));
      expect(parseRepositoryImageHref(image.href)).toMatchObject({ width: 16, height: 9, fluid });
    }
  });

  it('keeps an authored title', () => {
    const [image] = images(lexUnder('![shot](docs/a.png "Mine")', GITHUB, reader({ [KEY]: imageFound(KEY) })));
    expect(image.title).toBe('Mine');
  });

  it.each([
    ['pending', 'pending' as const],
    ['absent', { key: KEY, status: 'absent' } as ForgeFact],
    ['lfs', imageFound(KEY, { lfs: true, width: 0, height: 0 })],
  ])('claims it %s', (state, fact) => {
    const [image] = images(lexUnder('![shot](docs/a.png)', GITHUB, reader({ [KEY]: fact })));
    expect(parseRepositoryImageHref(image.href)?.state).toBe(state);
  });

  it.each([
    ['unavailable', { key: KEY, status: 'unavailable' } as ForgeFact],
    ['unread', undefined],
    ['found without an oid', { key: KEY, status: 'found' } as ForgeFact],
  ])('leaves it as written when %s', (_name, fact) => {
    const [image] = images(lexUnder('![shot](docs/a.png)', GITHUB, reader({ [KEY]: fact })));
    expect(image.href).toBe('docs/a.png');
  });

  it('leaves it as written without facts', () => {
    const [image] = images(lexUnder('![shot](docs/a.png)', GITHUB, null));
    expect(image.href).toBe('docs/a.png');
  });

  it('claims a GitLab project file URL by its branch', () => {
    const key = imageFactKey(BASE, 'a.png');
    const [image] = images(lexUnder('![x](https://gitlab.com/group/widget/-/raw/main/a.png)', GITLAB, reader({ [key]: imageFound(key) })));
    expect(parseRepositoryImageHref(image.href)).toMatchObject({ state: 'found', commit: BASE, path: 'a.png' });
    expect(parseRepositoryImageHref(image.href)?.browserUrl).toBe(`https://gitlab.com/group/widget/-/blob/${BASE}/a.png`);
  });

  it('reads the revision only for an image written by branch or relative to the PR', () => {
    const full = reader({}, undefined);
    lexUnder(`![x](../blob/${SHA}/a.png) ![y](https://example.com/y.png) text`, GITHUB, full);
    expect(full.read.has(REVISION_FACT_KEY)).toBe(false);
    expect(full.read.has(imageFactKey(SHA, 'a.png'))).toBe(true);
    const relative = reader({}, undefined);
    lexUnder('![x](docs/a.png)', GITHUB, relative);
    expect(relative.read.has(REVISION_FACT_KEY)).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// The scan names every key a lex reads
// ---------------------------------------------------------------------------

const P = (path: string, range: string) => `${GH}/blob/${SHA}/${path}#${range}`;
const SCANNED = [
  '![a](docs/a.png)',
  '![a](<docs/a b.png>)',
  '![a](docs/a\\_b.png)',
  '![a](docs/a&amp;b.png)',
  '![a][r]\n\n[r]: docs/r.png',
  '![r]\n\n[R]: <docs/r r.png> "t"',
  '- ![a][r]\n\n- [r]: ../blob/main/x.png',
  '> ![a](../raw/feature/x/q.png)',
  '[![a](docs/in-link.png)](https://example.com)',
  '<img src="docs/h.png" width="20">',
  "<p><img alt=x src='docs/h2.png'></p>",
  '<img src=docs/h3.png>',
  '<img src="docs/h&#46;png">',
  '<picture><source media="(prefers-color-scheme: dark)" srcset="docs/dark.png 2x, docs/x.png"><img src="docs/light.png"></picture>',
  '<div>\n\n![a](docs/region.png)\n\n</div>',
  '| h |\n| - |\n| ![a](docs/cell.png) |',
  `See ${P('src/x.ts', 'L3-L5')} for it.`,
  `${P('src/x.ts', 'L9-L4')}`,
  `- item ${P('a.go', 'L1')}\n- next`,
  `> quoted ${P('b.go', 'L2')}`,
  `Two ${P('c.go', 'L1')} and ${P('d.go', 'L3-L4')} on one line.`,
  `In parens (${P('e.go', 'L1')}).`,
  `Trailing ${P('f.go', 'L1')}.`,
  `<${P('g.go', 'L1')}>`,
  `[label](${P('h.go', 'L1')})`,
  `\`${P('i.go', 'L1')}\``,
  `Plain ${P('j.md', 'L3-L5').replace('#', '?plain=1#')} here.`,
  `Is it ${P('k.md', 'L2').replace('#', '?plain=1#')}?`,
  `Not ${P('l.md', 'L2').replace('#', '?plain=1&x=1#')} this.`,
];

// Bodies written to trip a scan that reads tags, destinations, labels or
// references differently from the parse: quoted `>` and `<`, quote kinds,
// unquoted values, case, line breaks and blockquote markers inside tags,
// references beyond the common five, escapes, `<…>` destinations,
// reference definitions in every container, pictures, images in links,
// code spans in labels, CRLF, and malformed tags, quotes and parentheses.
const ADVERSARIAL = [
  '![x][r]\r\r[r]: docs/cr.png',
  'Text\r![x](docs/cr2.png)\r\r<img\rsrc="docs/cr3.png">',
  '![a](docs/sp\\ x.png) ![a](docs/esc\\)x.png) ![a](docs/esc\\)y.png',
  "<img title=\"1 > 0\" src=\"docs/q1.png\">",
  "Text <img title=\"1 > 0\" src=\"docs/q2.png\"> text",
  "<p><img title=\"1 > 0\" src=\"docs/q3.png\"></p>",
  "<img src=\"docs/q4.png\" title=\"a > b\">",
  "<img alt='>' src='docs/q5.png'>",
  "<p><img alt='>' src='docs/q6.png' width=\"1>\"></p>",
  "<img src=\"docs/q>7.png\">",
  "<p><img src=\"docs/q>8.png\"></p>",
  "<img src=docs/u1.png alt=x>",
  "<p><img alt=x src=docs/u2.png></p>",
  "<IMG SRC=\"docs/up1.png\">",
  "<P><IMG ALT=\"x\" SrC='docs/up2.png'></P>",
  "<img\nsrc=\"docs/nl1.png\"\nalt=\"x\">",
  "<p>\n<img\n  title=\"a\nb > c\"\n  src=\"docs/nl2.png\">\n</p>",
  "Text <img\nsrc=\"docs/nl3.png\"> text",
  "> <img\n> src=\"docs/bq1.png\">",
  "> Text <img alt=\"x\"\n> src=\"docs/bq2.png\"> text",
  "- <img\n  src=\"docs/li1.png\">",
  "<img src=\"docs/a&sol;e1.png\">",
  "<p><img src=\"docs/e2&period;png\"></p>",
  "<img src=\"docs/e&lowbar;3.png\">",
  "<img src=\"docs/e4&colon;x.png\">",
  "<img src=\"docs/e5&#46;png\"> <img src=\"docs/e6&#x2E;png\">",
  "<p><img src=\"docs/e7&amp.png\"></p>",
  "<p><img src='docs/e8&ampx.png'></p>",
  "![a](docs/m&sol;e1.png) ![a](docs/m&period;e2.png) ![a](docs/m&lowbar;e3.png) ![a](docs/m&colon;e4.png) ![a](docs/m&NewLine;e5.png)",
  "![a](docs/m&#47;e6.png) ![a](docs/m&#x2F;e7.png) ![a](docs/m&#0;e8.png) ![a](docs/m&bogus;e9.png) ![a](docs/m&amp e10.png)",
  "![a](docs/s\\_1.png) ![a](docs/s\\(2\\).png) ![a](docs/s\\&amp;3.png) ![a](docs/s\\€4.png) ![a](docs/s\\a5.png)",
  "![a](<docs/w 1.png>) ![a](<docs/w 2.png> \"t\") ![a](< docs/w3.png >) ![a](<docs/w\\>4.png>) ![a](<docs/w<5.png>)",
  "![a](docs/t1.png \"title\") ![a](docs/t2.png 'title') ![a](docs/t3.png (title)) ![a](\n  docs/t4.png\n  \"t\")",
  "![r1]\n\n[r1]: docs/d1.png",
  "![x][]\n\n[x]: docs/d2.png",
  "![x][r3]\n\n[r3]: docs/d3.png \"t\"",
  "![x][R  4]\n\n[r 4]: <docs/d 4.png>",
  "> ![x][r5]\n>\n> [r5]: docs/d5.png",
  "- ![x][r6]\n- [r6]: docs/d6.png",
  "- item\n\n  ![x][r7]\n\n  [r7]:\n  docs/d7.png",
  "> - ![r8]\n>\n>   [r8]: docs/d8.png",
  "![r9]\n\n[r9]: docs/d&sol;9.png",
  "![r10]\n\n[r10]: <docs/d&period;10.png>",
  "![a\nb]\n\n[a\nb]: docs/d11.png",
  "![r12]\n\n- > [r12]: docs/d12.png",
  "![a][r13]\n\n[r13]: <docs/a>b13.png>",
  "<picture><source media=\"(prefers-color-scheme: dark)\" srcset=\"docs/p1.png 2x, docs/p1b.png\"><img src=\"docs/p1l.png\"></picture>",
  "<picture>\n<source media=\"(prefers-color-scheme: light)\" srcset=\" docs/p2&lowbar;x.png 1x\">\n<img src=\"docs/p2l.png\" alt=\"a > b\">\n</picture>",
  "<picture><SOURCE MEDIA='(prefers-color-scheme: dark)' SRCSET='docs/p3.png'><img src='docs/p3l.png'></picture>",
  "Text <picture><source media=\"(prefers-color-scheme: dark)\" srcset=\"docs/p4.png\"><img src=\"docs/p4l.png\"></picture> text",
  "[![a](docs/l1.png)](https://example.com)",
  "[![a][r14]](https://example.com)\n\n[r14]: docs/l2.png",
  "<a href=\"x\"><img src=\"docs/l3.png\"></a>",
  "[x <img src=\"docs/l4.png\"> y](https://example.com)",
  "<img src=\"docs/m1.png\"",
  "<img src=\"docs/m2.png",
  "<p><img alt=\"x src=\"docs/m3.png\"></p>",
  "![a](docs/(m4.png)",
  "![a](docs/m5.png))",
  "![a](docs/m6(1).png)",
  "![a](docs/m7.png",
  "![a `]` b](docs/c1.png)",
  "![a `[` b](docs/c2.png)",
  "![a ``]`` b][r15]\n\n[r15]: docs/c3.png",
  "> ![a](\n> docs/bq3.png)",
  "<img title=\"a\n\n<img src=\"docs/b2.png\">",
  "<p><img/src=\"docs/sl.png\"></p>",
  "<p><img alt=\"x\"src=\"docs/nows.png\"></p>",
  "<video src=\"docs/v.mp4\"></video>",
  "<p><video src=\"docs/v2.png\"></video></p>",
  "<video controls><source src=\"docs/v3.png\"></video>",
  "| h |\n| - |\n| <img title=\"a > b\" src=\"docs/cell.png\"> |",
  "<details><summary>s</summary>\n\n![a](docs/reg&sol;1.png)\n\n<img title=\"x>y\" src=\"docs/reg2.png\">\n\n</details>",
  "![a](docs/crlf1.png)\r\n\r\n![r16]\r\n\r\n[r16]: <docs/crlf2.png>\r\n",
  "![a](\r\n> docs/crlf3.png)",
  "<img title=\"1 > 0\" src=\"docs/a.png\">",
  "![x](docs/a&sol;b.png)",
  "![x](docs/a\\€b.png)",
  "![x](docs/a\\&amp;b.png)",
  "![x](<docs/a\\>b.png>)",
  "![a `]` b](docs/code.png)",
  "> ![a](\n> docs/bq.png)",
  "![a][r]\n\n[r]: <docs/a>b.png>",
  "![a\nb]\n\n[a\nb]: docs/ml.png",
  "![r]\n\n- > [r]: docs/lq.png",
  "<p><img/src=\"docs/slash.png\"></p>",
  "<IMG SRC=\"docs/up.png\">",
  "<p><img\nsrc=\"docs/nl.png\"\n></p>",
  "> <img\n> src=\"docs/bqimg.png\">",
  "> <p><img\n> src=\"docs/bqimg2.png\"></p>",
  "<p><video src=\"docs/v2.mp4\"></video></p>",
  "<p><img src=\"docs/a&amp.png\"></p>",
  "<p><img src=\"docs/a&period;png\"></p>",
  "<img src=\"docs/a&period;png\">",
  "<p><img alt=a<b src=\"docs/lt.png\"></p>",
  "![x](docs/a&NewLine;b.png)",
  "![x](docs/a&colon;b.png)",
  "<picture><source media=\"(prefers-color-scheme: dark)\" srcset=\" docs/d&lowbar;k.png 1x\"><img src=\"docs/l.png\"></picture>",
  "![a](docs/a.png \"t\")",
  "![a](docs/(p).png)",
  "[![a](docs/il.png)](docs/link.md)",
  "![a][R r]\n\n[r  R]: docs/fold.png",
  "![a][r\\]x]\n\n[r\\]x]: docs/esc.png",
];

describe('the repository file scan', () => {
  // A lex reads a paragraph's images; the sanitizer, run as find's
  // projection runs it under ChatMarkdown's options, reads an HTML block's.
  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('names every key a lex reads on %s', (_name, source) => {
    for (const rest of [undefined, 'pending' as const]) {
      for (const body of [...SCANNED, ...ADVERSARIAL]) {
        const facts = reader({}, rest);
        lexUnder(body, source, facts);
        markdownFindText(body, { embeddedHtml: true, forgeSource: source, facts: () => facts });
        const keys = new Set(forgeFactKeys(body, source, REVISION));
        expect([...facts.read].filter((key) => key !== REVISION_FACT_KEY && !keys.has(key)), body).toEqual([]);
      }
    }
  });

  it.each([
    ['a quoted `>` before the src', '<img title="1 > 0" src="docs/a.png">', 'docs/a.png'],
    ['a quoted `>` in an HTML block', '<p><img alt=\'>\' src=\'docs/a.png\'></p>', 'docs/a.png'],
    ['a `/` between attributes', '<p><img/src="docs/a.png"></p>', 'docs/a.png'],
    ['no space after a quoted value', '<p><img alt="x"src="docs/a.png"></p>', 'docs/a.png'],
    ['a tag across blockquote lines', '> <img\n> src="docs/a.png">', 'docs/a.png'],
    ['a video', '<video src="docs/a.mp4"></video>', 'docs/a.mp4'],
    ['a named reference beyond the five', '![x](docs/a&sol;b.png)', 'docs/a/b.png'],
    ['a named reference in an attribute', '<img src="docs/a&period;png">', 'docs/a.png'],
    ['a legacy reference in an attribute', '<p><img src="docs/a&amp.png"></p>', 'docs/a&.png'],
    ['a reference in a srcset', '<picture><source media="(prefers-color-scheme: dark)" srcset="docs/d&lowbar;k.png 1x"><img src="docs/l.png"></picture>', 'docs/d_k.png'],
    ['a non-ASCII escape', '![x](docs/a\\€b.png)', 'docs/a€b.png'],
    ['an escaped reference', '![x](docs/a\\&amp;b.png)', 'docs/a&amp;b.png'],
    ['an escaped `>` in `<…>`', '![x](<docs/a\\>b.png>)', 'docs/a>b.png'],
    ['an unclosed `(` the rule keeps', '![x](docs/(a.png)', 'docs/(a.png'],
    ['a `]` in a code span of the label', '![a `]` b](docs/a.png)', 'docs/a.png'],
    ['a destination on a blockquote\'s next line', '> ![a](\n> docs/a.png)', 'docs/a.png'],
    ['a `<…>` definition holding `>`', '![a][r]\n\n[r]: <docs/a>b.png>', 'docs/a>b.png'],
    ['a label across lines', '![a\nb]\n\n[a\nb]: docs/a.png', 'docs/a.png'],
    ['a definition in a list item\'s quote', '![r]\n\n- > [r]: docs/a.png', 'docs/a.png'],
    ['an escaped `]` in a reference', '![a][r\\]x]\n\n[r\\]x]: docs/a.png', 'docs/a.png'],
  ])('names the key the parse reads for %s', (_name, body, path) => {
    const facts = reader({}, 'pending');
    lexUnder(body, GITHUB, facts);
    markdownFindText(body, { embeddedHtml: true, forgeSource: GITHUB, facts: () => facts });
    const key = imageFactKey(HEAD, path);
    expect(facts.read.has(key)).toBe(true);
    expect(forgeFactKeys(body, GITHUB, REVISION)).toContain(key);
  });

  // Each family repeats a unit that opens a candidate some earlier one is
  // still reading (an unclosed quote, a tag in an unquoted value, a
  // destination in a destination, a label holding a code span), the shapes
  // that rescan the rest of the body once per candidate.
  it.each<[string, (n: number) => string]>([
    ['quotes that close at the next tag', (n) => '<img a="'.repeat(n)],
    ['mixed quotes', (n) => '<img a="<img b=\''.repeat(n)],
    ['one unclosed quote', (n) => `<img title="x${'<img '.repeat(n)}`],
    ['tags inside a quoted value', (n) => `<img a="${'<img b '.repeat(n)}">`],
    ['tags in an unquoted value', (n) => `${'<img/a=x'.repeat(n)}>`],
    ['unclosed tags', (n) => '<img src=docs/a.png '.repeat(n)],
    ['tags on blockquote lines', (n) => '> <img\n'.repeat(n)],
    ['nested destinations', (n) => `${'![a]('.repeat(n)}${')'.repeat(n)}`],
    ['unclosed nested destinations', (n) => '![a](x'.repeat(n)],
    ['destinations a space ends', (n) => '![a](x '.repeat(n)],
    ['image openers', (n) => '!['.repeat(n)],
    ['a code span in a label', (n) => `![\`${'](x) '.repeat(n)}`],
    ['definition lines', (n) => `![r]\n\n${'[r]: <a>b>c>d '.repeat(n)}`],
    ['unclosed `<…>` destinations', (n) => `![a](<${'x>'.repeat(n)}`],
  ])('walks each character a bounded number of times: %s', (_name, make) => {
    const perCharacter = (n: number) => {
      const body = make(n);
      repositoryScanCounters.steps = 0;
      forgeFactKeys(body, GITHUB, REVISION);
      return repositoryScanCounters.steps / body.length;
    };
    const small = perCharacter(250);
    // Linear holds the ratio; a rescan per candidate multiplies it by 8.
    expect(perCharacter(2000)).toBeLessThan(small * 2 + 1);
    expect(small).toBeLessThan(8);
  });

  it('names the keys each form writes', () => {
    const keys = (body: string) => forgeFactKeys(body, GITHUB, REVISION).filter((key) => key.startsWith('image:') || key.startsWith('lines:'));
    expect(keys('![a][r]\n\n[r]: ../blob/main/x.png')).toEqual([imageFactKey(BASE, 'x.png')]);
    expect(keys(SCANNED[13])).toEqual([imageFactKey(HEAD, 'docs/dark.png'), imageFactKey(HEAD, 'docs/light.png')]);
    expect(keys(SCANNED[20])).toEqual([linesFactKey(SHA, 'c.go', 1, 1), linesFactKey(SHA, 'd.go', 3, 4)]);
    expect(keys(SCANNED[17])).toEqual([linesFactKey(SHA, 'src/x.ts', 4, 9)]);
    expect(keys('![a](https://example.com/a.png)')).toEqual([]);
    expect(forgeFactKeys(`${P('x.go', 'L1')}`, GITLAB, REVISION).filter((key) => key.startsWith('lines:'))).toEqual([]);
  });

  it('does no destination or permalink work for a body with no candidate', () => {
    repositoryFileCounters.reads = 0;
    permalinkCounters.reads = 0;
    const plain = 'Plain words, a [link](https://example.com/x) and `code`.\n\n'.repeat(50);
    repositoryScanCounters.steps = 0;
    forgeFactKeys(plain, GITHUB, REVISION);
    expect(repositoryScanCounters.steps).toBe(0);
    lexUnder(plain, GITHUB, reader({}));
    expect(repositoryFileCounters.reads).toBe(0);
    expect(permalinkCounters.reads).toBe(0);
  });

  it('does work bounded by the candidates, not the text around them', () => {
    const measure = (filler: number) => {
      repositoryFileCounters.reads = 0;
      permalinkCounters.reads = 0;
      const body = `${'words '.repeat(filler)}![a](docs/a.png) ${P('x.go', 'L1')} ${'words '.repeat(filler)}`;
      forgeFactKeys(body, GITHUB, REVISION);
      lexUnder(body, GITHUB, reader({}, 'pending'));
      return [repositoryFileCounters.reads, permalinkCounters.reads];
    };
    const small = measure(10);
    expect(measure(5000)).toEqual(small);
    // Once each for the scan, the document context's lex of the block and
    // the block's own lex.
    expect(small).toEqual([3, 3]);
  });

  it('marks a body whose images name the head as reading the head, and a permalink-only body as not', () => {
    const scanner = (body: string) => forgeFactKeys(body, GITHUB, REVISION);
    expect(scanner('![a](docs/a.png)')).toContain(imageFactKey(HEAD, 'docs/a.png'));
    // A head change renames that key; a permalink's never does.
    expect(forgeFactKeys('![a](docs/a.png)', GITHUB, { ...REVISION, head: SHA })).toEqual([imageFactKey(SHA, 'docs/a.png')]);
    expect(forgeFactKeys(P('x.go', 'L1'), GITHUB, { ...REVISION, head: SHA })).toEqual(scanner(P('x.go', 'L1')));
  });
});

// ---------------------------------------------------------------------------
// The permalink snippet
// ---------------------------------------------------------------------------

const LINES = linesFactKey(SHA, 'src/x.ts', 3, 5);
const linesFound = (key: string, text: string): ForgeFact => ({ key, status: 'found', oid: OID, text, path: 'src/x.ts' });
const shape = (tokens: readonly Token[]) => (tokens as AnyToken[]).map((token) => token.type);

describe('a permalink snippet', () => {
  const URL3 = P('src/x.ts', 'L3-L5');

  it('stands as a block between the text around it', () => {
    const body = `Before ${URL3} after.`;
    const tokens = lexUnder(body, GITHUB, reader({ [LINES]: linesFound(LINES, 'a\nb\nc') }));
    expect(shape(tokens)).toEqual(['paragraph', 'codeSnippet', 'paragraph']);
    expect(tokens.map((token) => token.raw).join('')).toBe(body);
    const [before, snippet, after] = tokens as AnyToken[];
    // The run beside the block shows without the space next to it.
    expect((before.tokens as AnyToken[]).map((token) => (token as { text: string }).text)).toEqual(['Before']);
    expect((after.tokens as AnyToken[]).map((token) => (token as { text: string }).text)).toEqual(['after.']);
    const [header, code] = snippet.tokens as AnyToken[];
    expect(code).toMatchObject({ type: 'code', lang: 'ts', text: 'a\nb\nc', firstLine: 3 });
    const links: AnyToken[] = [];
    each(header.tokens ?? [], (token) => token.type === 'link' && links.push(token));
    expect(links.map((link) => link.href)).toEqual([URL3, `${GH}/commit/${SHA}`]);
    const texts: string[] = [];
    each(header.tokens ?? [], (token) => (token.type === 'text' || token.type === 'codespan') && !token.tokens && texts.push((token as { text: string }).text));
    expect(texts).toEqual(['widget/src/x.ts', 'Lines 3 to 5 in ', SHA.slice(0, 7)]);
  });

  it('says one line as one', () => {
    const key = linesFactKey(SHA, 'src/x.ts', 4, 4);
    const [snippet] = lexUnder(P('src/x.ts', 'L4'), GITHUB, reader({ [key]: linesFound(key, 'only') })) as AnyToken[];
    const texts: string[] = [];
    each(snippet.tokens ?? [], (token) => token.type === 'text' && texts.push((token as { text: string }).text));
    expect(texts).toContain('Line 4 in ');
  });

  it('frames the range with empty lines while it is asked about', () => {
    const [snippet] = lexUnder(URL3, GITHUB, reader({ [LINES]: 'pending' })) as AnyToken[];
    expect(snippet.type).toBe('codeSnippet');
    expect((snippet.tokens as AnyToken[])[1]).toMatchObject({ type: 'code', lang: '', text: '\n\n', firstLine: 3 });
  });

  it('stands as a block for a `?plain=1` permalink, its header linking the URL as written', () => {
    const url = `${GH}/blob/${SHA}/README.md?plain=1#L3-L5`;
    const key = linesFactKey(SHA, 'README.md', 3, 5);
    const body = `See ${url} here.`;
    const facts = reader({ [key]: linesFound(key, 'a\nb\nc') });
    const tokens = lexUnder(body, GITHUB, facts);
    expect(facts.read.has(key)).toBe(true);
    expect(forgeFactKeys(body, GITHUB, REVISION)).toContain(key);
    expect(shape(tokens)).toEqual(['paragraph', 'codeSnippet', 'paragraph']);
    const links: AnyToken[] = [];
    each((tokens[1] as AnyToken).tokens ?? [], (token) => token.type === 'link' && links.push(token));
    expect(links.map((link) => link.href)).toEqual([url, `${GH}/commit/${SHA}`]);
  });

  it.each(['?plain=0', '?plain=1&x=1', '?raw=1'])('stays an ordinary link with the query %s', (query) => {
    const url = `${GH}/blob/${SHA}/README.md${query}#L3-L5`;
    const facts = reader({}, 'pending');
    expect(shape(lexUnder(`See ${url}`, GITHUB, facts))).toEqual(['paragraph']);
    expect([...facts.read].filter((key) => key.startsWith('lines:'))).toEqual([]);
  });

  it('reads a reversed range as the same key', () => {
    const facts = reader({ [LINES]: linesFound(LINES, 'a\nb\nc') });
    expect(shape(lexUnder(P('src/x.ts', 'L5-L3'), GITHUB, facts))).toEqual(['codeSnippet']);
    expect(facts.read.has(LINES)).toBe(true);
  });

  it.each([
    ['in a list item', `- one ${URL3} two\n- next`, ['list']],
    ['in a quote', `> quoted ${URL3}`, ['blockquote']],
    ['twice on one line', `A ${URL3} and ${URL3} end`, ['paragraph', 'codeSnippet', 'paragraph', 'codeSnippet', 'paragraph']],
    ['alone after a line', `Intro line\n${URL3}`, ['paragraph', 'codeSnippet']],
  ])('splits its paragraph %s', (_name, body, top) => {
    const tokens = lexUnder(body, GITHUB, reader({ [LINES]: linesFound(LINES, 'a\nb\nc') }));
    expect(shape(tokens)).toEqual(top);
    const kinds: string[] = [];
    each(tokens, (token) => kinds.push(token.type));
    expect(kinds).toContain('codeSnippet');
    let raw = 0;
    each(tokens, (token) => token.type === 'codeSnippet' && (raw += token.raw === URL3 ? 1 : 0));
    expect(raw).toBe(body.split(URL3).length - 1);
  });

  it.each([
    ['as a labeled link', `[label](${URL3})`],
    ['in emphasis', `*see ${URL3}*`],
    ['in a heading', `# ${URL3}`],
    ['in a table cell', `| h |\n| - |\n| ${URL3} |`],
    ['as code', `\`${URL3}\``],
  ])('stays inline %s', (_name, body) => {
    const kinds: string[] = [];
    each(lexUnder(body, GITHUB, reader({ [LINES]: linesFound(LINES, 'a\nb\nc') })), (token) => kinds.push(token.type));
    expect(kinds).not.toContain('codeSnippet');
  });

  it.each([
    ['absent', { key: LINES, status: 'absent' } as ForgeFact],
    ['unavailable', { key: LINES, status: 'unavailable' } as ForgeFact],
    ['unread', undefined],
  ])('stays an ordinary link when %s', (_name, fact) => {
    const tokens = lexUnder(`See ${URL3}`, GITHUB, reader({ [LINES]: fact }));
    expect(shape(tokens)).toEqual(['paragraph']);
    const links: AnyToken[] = [];
    each(tokens, (token) => token.type === 'link' && links.push(token));
    expect(links.map((link) => link.href)).toEqual([URL3]);
  });

  it('stays an ordinary link without facts and on GitLab', () => {
    expect(shape(lexUnder(`See ${URL3}`, GITHUB, null))).toEqual(['paragraph']);
    const key = linesFactKey(SHA, 'x.ts', 1, 1);
    expect(shape(lexUnder(`See https://gitlab.com/group/widget/-/blob/${SHA}/x.ts#L1`, GITLAB, reader({ [key]: linesFound(key, 'x') })))).toEqual(['paragraph']);
  });
});
