import { describe, expect, it } from 'vitest';
import { createDocumentContextCache, createParseBlocksCache, documentContextOf, lex, parseBlocks, type ForgeFactReader, type Token } from '../markdown';
import { chatMarkdownExtensions, chatMarkdownForgeOptions } from '../components/chat/markdown/chatMarkdownOptions';
import type { ForgeAttachmentSource } from './forgeAttachments';
import { imageFactKey, linesFactKey, NO_REVISION, REVISION_FACT_KEY, type FactRevision, type ForgeFact, type ForgeFactState } from './forgeFacts';
import { forgeFactKeys } from './forgeReferenceExtension';
import { MAX_SNIPPET_LINES, permalinkCounters, permalinkOf, repositoryImageOf, snippetLanguage } from './forgeRepositoryFiles';
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
  ])('reads %s', (href, start, end) => {
    expect(permalinkOf(href, shapes)).toEqual({ sha: SHA, path: 'src/x.ts', start, end, repo: 'widget', owner: 'acme' });
  });

  it.each([
    [`${GH}/blob/${SHA.slice(0, 7)}/src/x.ts#L3`, 'an abbreviated sha'],
    [`${GH}/blob/main/src/x.ts#L3`, 'a branch'],
    [`https://github.com/other/widget/blob/${SHA}/src/x.ts#L3`, 'another repository'],
    [`${GH}/blob/${SHA}/src/x.ts`, 'no fragment'],
    [`${GH}/blob/${SHA}/src/x.ts#L1-L${MAX_SNIPPET_LINES + 1}`, 'too many lines'],
    [`${GH}/blob/${SHA}/src/x.ts?plain=1#L3`, 'a query'],
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
];

describe('the repository file scan', () => {
  it.each([['GitHub', GITHUB], ['GitLab', GITLAB]] as const)('names every key a lex reads on %s', (_name, source) => {
    for (const rest of [undefined, 'pending' as const]) {
      for (const body of SCANNED) {
        const facts = reader({}, rest);
        lexUnder(body, source, facts);
        const keys = new Set(forgeFactKeys(body, source, REVISION));
        expect([...facts.read].filter((key) => key !== REVISION_FACT_KEY && !keys.has(key)), body).toEqual([]);
      }
    }
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
    forgeFactKeys(plain, GITHUB, REVISION);
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
