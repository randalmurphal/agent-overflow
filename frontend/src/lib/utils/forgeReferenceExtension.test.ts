import { describe, expect, it } from 'vitest';
import { lex, type Extension, type Token } from '../markdown';
import { chatMarkdownExtensions, chatMarkdownForgeOptions } from '../components/chat/markdown/chatMarkdownOptions';
import { buildForgeReferences } from './forgeReferenceExtension';
import type { ForgeAttachmentSource } from './forgeAttachments';

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
const GH = 'https://github.com/acme/widget';
const GL = 'https://gitlab.com/group/widget';
const SHA = '0123456789abcdef0123456789abcdef01234567';

/** ChatMarkdown's extension list for a review surface on `source`'s forge. */
function extensionsFor(source: ForgeAttachmentSource | null): Extension[] {
  return chatMarkdownExtensions({ forge: chatMarkdownForgeOptions(source, true).extensions, embeddedHtml: true });
}

interface Shown {
  text: string;
  href: string;
  code?: true;
}

function linksIn(tokens: readonly Token[], out: Shown[] = []): Shown[] {
  for (const token of tokens) {
    if (token.type === 'link') {
      const children = token.tokens ?? [];
      const text = children.map((child) => ('text' in child ? child.text : '')).join('');
      out.push(children.some((child) => child.type === 'codespan') ? { text, href: token.href, code: true } : { text, href: token.href });
      // A link inside link text is listed too, so a nested anchor fails.
      linksIn(children, out);
      continue;
    }
    const nested = (token as { tokens?: Token[]; items?: Token[] });
    if (nested.tokens) linksIn(nested.tokens, out);
    if (nested.items) linksIn(nested.items, out);
  }
  return out;
}

function links(markdown: string, source: ForgeAttachmentSource | null): Shown[] {
  return linksIn(lex(markdown, extensionsFor(source)) as unknown as Token[]);
}

describe('forge references on a GitHub surface', () => {
  it.each([
    ['#12', { text: '#12', href: `${GH}/issues/12` }],
    ['GH-12', { text: 'GH-12', href: `${GH}/issues/12` }],
    ['acme/widget#12', { text: '#12', href: `${GH}/issues/12` }],
    ['other/repo#7', { text: 'other/repo#7', href: 'https://github.com/other/repo/issues/7' }],
    ['Closes #12.', { text: '#12', href: `${GH}/issues/12` }],
    ['(#12)', { text: '#12', href: `${GH}/issues/12` }],
    ['**x** other/repo#7', { text: 'other/repo#7', href: 'https://github.com/other/repo/issues/7' }],
  ])('links %s', (markdown, expected) => {
    expect(links(markdown, GITHUB)).toEqual([expected]);
  });

  it.each([
    '\\#12', '`#12`', 'issue#12', '#12a', '#0', '!12', 'GL-12', 'a/b/c#12', 'widget#12', '&#12;', 'x@y',
  ])('leaves %s as text', (markdown) => {
    expect(links(markdown, GITHUB)).toEqual([]);
  });

  it('never links inside link text or a raw anchor', () => {
    expect(links('[see #12 and @octo](https://example.invalid/one)', GITHUB)).toEqual([
      { text: 'see #12 and @octo', href: 'https://example.invalid/one' },
    ]);
    expect(links('<a href="https://example.invalid/two">see #12</a>', GITHUB)).toEqual([
      { text: 'see #12', href: 'https://example.invalid/two' },
    ]);
  });

  it('never links inside code or an HTML attribute', () => {
    expect(links('```\n#12 @octo\n```\n\n<span title="#12 @octo">x</span>', GITHUB)).toEqual([]);
  });

  it('links commits, in monospace, as GitHub shows them', () => {
    expect(links(`Full ${SHA}, short 0123abc, 0123abc...4567def0`, GITHUB)).toEqual([
      { text: '0123456', href: `${GH}/commit/${SHA}`, code: true },
      { text: '0123abc', href: `${GH}/commit/0123abc`, code: true },
      { text: '0123abc...4567def', href: `${GH}/compare/0123abc...4567def0`, code: true },
    ]);
    expect(links(`acme@${SHA} acme/widget@0123abc other@0123abc other/repo@0123abc`, GITHUB)).toEqual([
      { text: '0123456', href: `${GH}/commit/${SHA}`, code: true },
      { text: '0123abc', href: `${GH}/commit/0123abc`, code: true },
      { text: 'other/widget@0123abc', href: 'https://github.com/other/widget/commit/0123abc', code: true },
      { text: 'other/repo@0123abc', href: 'https://github.com/other/repo/commit/0123abc', code: true },
    ]);
  });

  it.each([
    ['a number', '1234567'],
    ['a word', 'deadbeef'],
    ['six hex digits', '0123ab'],
    ['part of a UUID', '0123abcd-0123-4567-89ab-0123456789ab'],
    ['part of a file name', '0123abcd.txt'],
    ['a hex literal', '0x0123abcd'],
    ['a color', '#0123abc'],
    ['glued to a word', 'v0123abcd'],
  ])('leaves %s as text', (_name, markdown) => {
    expect(links(markdown, GITHUB)).toEqual([]);
  });

  it('links mentions, and a team of the repository owner only', () => {
    expect(links('@octo-cat, **@octo** @acme/core @other/core @/ent:core foo@bar \\@octo', GITHUB)).toEqual([
      { text: '@octo-cat', href: 'https://github.com/octo-cat' },
      { text: '@octo', href: 'https://github.com/octo' },
      { text: '@acme/core', href: 'https://github.com/orgs/acme/teams/core' },
    ]);
  });

  it('shows a pasted issue, PR, comment, commit or compare URL in short form', () => {
    const source = [
      `${GH}/issues/12`,
      `<${GH}/pull/12>`,
      `${GH}/pull/12#issuecomment-99`,
      `https://github.com/other/repo/pull/7`,
      `${GH}/commit/${SHA}`,
      `${GH}/compare/main...0123abc`,
    ].join('\n');
    expect(links(source, GITHUB)).toEqual([
      { text: '#12', href: `${GH}/issues/12` },
      { text: '#12', href: `${GH}/pull/12` },
      { text: '#12 (comment)', href: `${GH}/pull/12#issuecomment-99` },
      { text: 'other/repo#7', href: 'https://github.com/other/repo/pull/7' },
      { text: '0123456', href: `${GH}/commit/${SHA}`, code: true },
      { text: 'main...0123abc', href: `${GH}/compare/main...0123abc`, code: true },
    ]);
  });

  it.each([
    `${GH}/pull/12/files`,
    `${GH}/pull/12?tab=files`,
    `${GH}/labels/bug`,
    'https://redirect.github.com/acme/widget/issues/12',
    `${GL}/-/merge_requests/3`,
  ])('keeps %s as written', (url) => {
    expect(links(url, GITHUB)).toEqual([{ text: url, href: url }]);
  });

  it('resolves a relative link against the PR page, as GitHub leaves it for the browser', () => {
    expect(links('[a](docs/a.md) [b](../blob/main/b.png?raw=true) [c](/other/repo) [d](?tab=files) [e](#frag)', GITHUB)).toEqual([
      { text: 'a', href: `${GH}/pull/docs/a.md` },
      { text: 'b', href: `${GH}/blob/main/b.png?raw=true` },
      { text: 'c', href: 'https://github.com/other/repo' },
      { text: 'd', href: `${GH}/pull/4?tab=files` },
      { text: 'e', href: '#frag' },
    ]);
  });
});

describe('forge references on a GitLab surface', () => {
  it.each([
    ['#12', { text: '#12', href: `${GL}/-/issues/12` }],
    ['!12', { text: '!12', href: `${GL}/-/merge_requests/12` }],
    ['group/widget!12', { text: '!12', href: `${GL}/-/merge_requests/12` }],
    ['widget#12', { text: '#12', href: `${GL}/-/issues/12` }],
    ['group/other!12', { text: 'other!12', href: 'https://gitlab.com/group/other/-/merge_requests/12' }],
    ['team/sub/proj#5', { text: 'team/sub/proj#5', href: 'https://gitlab.com/team/sub/proj/-/issues/5' }],
    ['!12+ next', { text: '!12', href: `${GL}/-/merge_requests/12` }],
    ['#12+s next', { text: '#12', href: `${GL}/-/issues/12` }],
    ['group/widget>', { text: 'group/widget>', href: GL }],
    ['@dev.one.', { text: '@dev.one', href: 'https://gitlab.com/dev.one' }],
    ['@group/sub', { text: '@group/sub', href: 'https://gitlab.com/group/sub' }],
  ])('links %s', (markdown, expected) => {
    expect(links(markdown, GITLAB)).toEqual([expected]);
  });

  it.each(['a#12', 'pkg!12', 'other#12', 'GH-12', '\\!12', '$1', '%1', '&1', '~bug', '100%1'])('leaves %s as text', (markdown) => {
    expect(links(markdown, GITLAB)).toEqual([]);
  });

  it('consumes the expansion suffix with the reference', () => {
    const [paragraph] = lex('!12+ and #3+s', extensionsFor(GITLAB)) as unknown as { tokens: Token[] }[];
    expect(paragraph.tokens.map((token) => token.raw)).toEqual(['!12+', ' and ', '#3+s']);
  });

  it('links commits by their first eight characters, in plain text', () => {
    expect(links(`${SHA} 0123abcd group/widget@0123abcd group/other@${SHA}...${SHA}`, GITLAB)).toEqual([
      { text: '01234567', href: `${GL}/-/commit/${SHA}` },
      { text: '0123abcd', href: `${GL}/-/commit/0123abcd` },
      { text: '0123abcd', href: `${GL}/-/commit/0123abcd` },
      { text: 'other@01234567...01234567', href: `https://gitlab.com/group/other/-/compare/${SHA}...${SHA}` },
    ]);
  });

  it('shows a pasted MR, issue or commit URL in short form', () => {
    const source = [
      `${GL}/-/merge_requests/12`,
      `${GL}/-/merge_requests/12/diffs`,
      `${GL}/-/issues/5+`,
      `https://gitlab.com/group/other/-/issues/5`,
      `${GL}/-/commit/${SHA}`,
    ].join('\n');
    expect(links(source, GITLAB)).toEqual([
      { text: '!12', href: `${GL}/-/merge_requests/12` },
      { text: '!12 (diffs)', href: `${GL}/-/merge_requests/12/diffs` },
      { text: '#5', href: `${GL}/-/issues/5` },
      { text: 'other#5', href: 'https://gitlab.com/group/other/-/issues/5' },
      { text: '01234567', href: `${GL}/-/commit/${SHA}` },
    ]);
  });

  it.each([`${GL}/-/issues/5#note_1`, `${GL}/-/blob/main/README.md`, `${GH}/pull/4`])('keeps %s as written', (url) => {
    expect(links(url, GITLAB)).toEqual([{ text: url, href: url }]);
  });

  it('resolves a relative link in the repository, as GitLab does', () => {
    expect(links('[a](docs/a.md#L3) [b](../README.md) [c](/src/x.go) [d](?tab=files) [e](#frag)', GITLAB)).toEqual([
      { text: 'a', href: `${GL}/-/blob/HEAD/docs/a.md#L3` },
      { text: 'b', href: `${GL}/-/blob/README.md` },
      { text: 'c', href: `${GL}/-/blob/HEAD/src/x.go` },
      { text: 'd', href: `${GL}/-/merge_requests/3?tab=files` },
      { text: 'e', href: '#frag' },
    ]);
  });

  it('leaves an upload link to the attachment extension', () => {
    const [link] = links(`[log](/uploads/${'a'.repeat(32)}/build.log)`, GITLAB);
    expect(link.href.startsWith('agent-overflow:forge?')).toBe(true);
  });

  it('links wiki pages, but not the table of contents marker', () => {
    expect(links('[[Home]] [[Two Words]] [[Read me|read-me]] [[_TOC_]]', GITLAB)).toEqual([
      { text: 'Home', href: `${GL}/-/wikis/Home` },
      { text: 'Two Words', href: `${GL}/-/wikis/Two-Words` },
      { text: 'Read me', href: `${GL}/-/wikis/read-me` },
    ]);
    expect(links('[[Home]]', GITHUB)).toEqual([]);
  });

  it('links a URL of any openable scheme', () => {
    expect(links('smb://host/share, irc://irc.example.invalid/ javascript://x', GITLAB)).toEqual([
      { text: 'smb://host/share', href: 'smb://host/share' },
      { text: 'irc://irc.example.invalid/', href: 'irc://irc.example.invalid/' },
    ]);
    expect(links('smb://host/share', GITHUB)).toEqual([]);
  });
});

describe('extended autolinks on forge surfaces', () => {
  it.each([GITHUB, GITLAB])('links email addresses as the forges do (%#)', (source) => {
    expect(links('a@b.example, a+tag@b.example. <c@d.example> mailto:e@f.example xmpp:g@h.example/res x_@y.z_ foo@bar', source)).toEqual([
      { text: 'a@b.example', href: 'mailto:a@b.example' },
      { text: 'a+tag@b.example', href: 'mailto:a+tag@b.example' },
      { text: 'c@d.example', href: 'mailto:c@d.example' },
      { text: 'mailto:e@f.example', href: 'mailto:e@f.example' },
      { text: 'xmpp:g@h.example/res', href: 'xmpp:g@h.example/res' },
    ]);
  });
});

describe('agent chat', () => {
  it('links no reference, mention, email or relative link', () => {
    expect(links(`#12 !12 @octo a@b.example <c@d.example> ${SHA} [[Home]] smb://x/y`, null)).toEqual([]);
    expect(links('[a](docs/a.md)', null)).toEqual([{ text: 'a', href: 'docs/a.md' }]);
  });
});

describe('the tokenizer contract', () => {
  it('claims nothing for a PR with no forge address', () => {
    const { extension } = buildForgeReferences({ pr: { ...GITHUB.pr, host: '' }, backend: 'home', webBase: '' });
    expect(linksIn(lex('#12 @octo a@b.example', [extension]) as unknown as Token[])).toEqual([]);
  });

  it.each([GITHUB, GITLAB])('keeps an inline run\'s raws summing to its source (%#)', (source) => {
    const markdown = `See #12, !12+, acme/widget#3, @octo., a@b.example, ${SHA}...${SHA}, group/widget>, [a](docs/a.md) and [[Home]].`;
    const [paragraph] = lex(markdown, extensionsFor(source)) as unknown as { raw: string; tokens: Token[] }[];
    expect(paragraph.tokens.map((token) => token.raw).join('')).toBe(markdown);
  });

  it('asks the tokenizer once per candidate inside a long word, not once per character', () => {
    const { extension } = buildForgeReferences(GITHUB);
    let calls = 0;
    const counted: Extension = {
      ...extension,
      tokenizer(src, tokens) {
        calls++;
        return extension.tokenizer!.call(this, src, tokens);
      },
    };
    // The strikethrough ends mid-word, so the text run after it opens
    // inside a word whose later `#` is a candidate.
    const tokens = lex(`~~a~~${'b'.repeat(4000)}/x#1`, [counted]) as unknown as Token[];
    expect(linksIn(tokens)).toEqual([]);
    expect(calls).toBeLessThan(50);
  });
});
