import { describe, expect, it, vi } from 'vitest';
import { destinationsOf, embeddedHtmlFor, Lexer, markedImageAttributes, type Extension, type Token } from '../markdown';
import { buildForgeAttachmentExtension } from './forgeAttachmentExtension';
import { parseForgeAttachmentHref } from './forgeAttachments';
import { buildPathLinkExtension } from './pathLinkExtension';
import type { PRRef } from './prReference';

vi.mock('../native/platform', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../native/platform')>()),
  isNativeShell: () => false,
}));

const HEX = '0123456789abcdef0123456789abcdef';
const GITHUB_PR: PRRef = { forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widget', number: 7 };
const GITLAB_MR: PRRef = { forge: 'gitlab', host: 'gitlab.com', namespace: 'group/sub', repo: 'widget', number: 3 };
const GITLAB_WEB = 'https://gitlab.example.test/group/sub/widget/-/merge_requests/3';

type Ext = { level: 'inline' | 'block'; tokenizer?: unknown; start?: unknown };

/**
 * Drive the real lexer with the extension list ChatMarkdown builds, in the
 * same order. The order is the contract: the path-link and embedded-HTML
 * extensions claim the same starts (`[`, `![`, `<`), so a forge token only
 * survives because the forge extension is asked first.
 */
function lex(text: string, extensions: Ext[]): Token[] {
  const lexer = new Lexer({
    gfm: true,
    extensions: {
      block: extensions.filter((e) => e.level === 'block' && e.tokenizer).map((e) => e.tokenizer),
      inline: extensions.filter((e) => e.level === 'inline' && e.tokenizer).map((e) => e.tokenizer),
      childTokens: {},
      renderers: {},
      startBlock: [],
      startInline: extensions.map((e) => e.start).filter(Boolean),
    },
    destinations: destinationsOf(extensions as Extension[]),
  } as never);
  return lexer.lex(text) as Token[];
}

function forgeExtensions(pr: PRRef, webBase = '', embeddedHtml = true): Ext[] {
  return buildForgeAttachmentExtension({
    pr,
    backend: 'gpu',
    webBase,
    embeddedHtml,
  }) as unknown as Ext[];
}

function fullStack(pr: PRRef, webBase = '', embeddedHtml = true): Ext[] {
  const list: Ext[] = [...forgeExtensions(pr, webBase, embeddedHtml)];
  const pathLink = buildPathLinkExtension([], '/repo', 'editor');
  if (pathLink) list.push(pathLink as unknown as Ext);
  if (embeddedHtml) list.push(...(embeddedHtmlFor(pr.forge).extensions as unknown as Ext[]));
  return list;
}

interface Claimed {
  type: string;
  raw: string;
  text: string;
  title: string | null;
  /** The original href the forge token carries, or null when not ours. */
  href: string | null;
}

function claimed(tokens: readonly Token[]): Claimed[] {
  const out: Claimed[] = [];
  const walk = (list: readonly Token[]): void => {
    for (const token of list) {
      const t = token as { type: string; raw: string; text?: string; title?: string | null; href?: string; tokens?: Token[] };
      if (t.type === 'image' || t.type === 'link') {
        const parsed = parseForgeAttachmentHref(t.href);
        if (parsed) {
          out.push({
            type: t.type,
            raw: t.raw,
            text: t.text ?? '',
            title: t.title ?? null,
            href: parsed.href,
          });
        }
      }
      if (Array.isArray(t.tokens)) walk(t.tokens);
    }
  };
  walk(tokens);
  return out;
}

describe('markdown images and links', () => {
  it('claims a gitlab upload image and keeps its alt text', () => {
    expect(claimed(lex(`![a shot](/uploads/${HEX}/shot.png)`, fullStack(GITLAB_MR, GITLAB_WEB)))).toEqual([
      {
        type: 'image',
        raw: `![a shot](/uploads/${HEX}/shot.png)`,
        text: 'a shot',
        title: `https://gitlab.example.test/group/sub/widget/uploads/${HEX}/shot.png`,
        href: `/uploads/${HEX}/shot.png`,
      },
    ]);
  });

  // The list is markedImageAttributes', which ChatMarkdown adds after the
  // claims on a GitLab surface: it sizes whatever claimed the image.
  it.each([
    ['a markdown image', `![a shot](/uploads/${HEX}/shot.png)`],
    ['an embedded <img>', `<img src="/uploads/${HEX}/shot.png" alt="a shot">`],
    ['a video', `<video src="/uploads/${HEX}/clip.mp4"></video>`],
  ])('is sized by a gitlab attribute list after %s', (_name, embed) => {
    const source = `See ${embed}{width=30 height=20px} after`;
    const tokens = lex(source, [...fullStack(GITLAB_MR, GITLAB_WEB), markedImageAttributes as unknown as Ext]);
    const inline = (tokens[0] as { tokens: Array<{ type: string; raw: string; href: string; width?: string; height?: string }> }).tokens;
    expect(inline.map((t) => [t.type, t.raw])).toEqual([
      ['text', 'See '],
      ['image', embed],
      ['space', '{width=30 height=20px}'],
      ['text', ' after'],
    ]);
    expect(parseForgeAttachmentHref(inline[1].href)).not.toBeNull();
    expect([inline[1].width, inline[1].height]).toEqual(['30', '20']);
  });

  it('reads no attribute list on a github pull request', () => {
    const asset = 'https://github.com/user-attachments/assets/abc';
    const tokens = lex(`![a](${asset}){width=30}`, fullStack(GITHUB_PR));
    const inline = (tokens[0] as { tokens: Array<{ type: string; raw: string; width?: string }> }).tokens;
    expect(inline.map((t) => [t.type, t.raw])).toEqual([['image', `![a](${asset})`], ['text', '{width=30}']]);
    expect(inline[0].width).toBeUndefined();
  });

  it('falls back to the attachment name when the image has no alt', () => {
    const [token] = claimed(lex(`![](/uploads/${HEX}/shot.png)`, fullStack(GITLAB_MR)));
    expect(token.text).toBe('shot.png');
  });

  it('claims a file link and titles it with the action the click will take', () => {
    expect(claimed(lex(`[the report](/uploads/${HEX}/report.pdf)`, fullStack(GITLAB_MR)))).toEqual([
      {
        type: 'link',
        raw: `[the report](/uploads/${HEX}/report.pdf)`,
        text: 'the report',
        title: 'Download report.pdf',
        href: `/uploads/${HEX}/report.pdf`,
      },
    ]);
  });

  it('leaves every other link and image to the extensions behind it', () => {
    const source = [
      '[docs](docs/guide.md)',
      '![logo](https://example.test/logo.png)',
      `[other project](/group/other/uploads/${HEX}/x.png)`,
    ].join('\n\n');
    expect(claimed(lex(source, fullStack(GITLAB_MR, GITLAB_WEB)))).toEqual([]);
  });

  it('does not claim a gitlab upload while rendering a github pull request', () => {
    expect(claimed(lex(`![x](/uploads/${HEX}/shot.png)`, fullStack(GITHUB_PR)))).toEqual([]);
  });

  it('leaves a forge reference inside a code span alone', () => {
    expect(claimed(lex(`\`![x](/uploads/${HEX}/shot.png)\``, fullStack(GITLAB_MR)))).toEqual([]);
  });
});

describe('bare GitHub attachment URLs', () => {
  it('becomes an image token, which is how the forge spells both video and picture', () => {
    const url = 'https://github.com/user-attachments/assets/2b6d-0f0e';
    expect(claimed(lex(url, fullStack(GITHUB_PR)))).toEqual([
      {
        type: 'image',
        raw: url,
        text: '2b6d-0f0e',
        title: url,
        href: url,
      },
    ]);
  });

  it('stops at the sentence punctuation that follows it', () => {
    const tokens = claimed(lex('See https://github.com/user-attachments/assets/abc.', fullStack(GITHUB_PR)));
    expect(tokens.map((t) => t.href)).toEqual(['https://github.com/user-attachments/assets/abc']);
    expect(tokens[0].raw).toBe('https://github.com/user-attachments/assets/abc');
  });

  it('claims a signed private-user-images redirect', () => {
    const url = 'https://private-user-images.githubusercontent.com/1/shot.png?jwt=abc';
    expect(claimed(lex(url, fullStack(GITHUB_PR))).map((t) => t.href)).toEqual([url]);
  });

  it('leaves an ordinary github URL as marked autolinked it', () => {
    expect(claimed(lex('https://github.com/acme/widget/pull/7', fullStack(GITHUB_PR)))).toEqual([]);
  });

  it('does not claim a github URL inside a gitlab merge request', () => {
    expect(claimed(lex('https://github.com/user-attachments/assets/x', fullStack(GITLAB_MR)))).toEqual([]);
  });

  it('bounds how far inline text may run, so the URL reaches the tokenizer', () => {
    const [inline] = buildForgeAttachmentExtension({
      pr: GITHUB_PR,
      backend: 'gpu',
      webBase: '',
      embeddedHtml: true,
    });
    const start = (src: string) => inline.start!.call({} as never, src);
    expect(start('text https://github.com/user-attachments/assets/x')).toBe(5);
    expect(start('no attachment here')).toBeUndefined();
    // Glued into a longer word: not a reference, so not a bound.
    expect(start('xhttps://github.com/user-attachments/assets/x')).toBeUndefined();
  });
});

describe('embedded HTML', () => {
  it('claims an <img> src the forge serves', () => {
    const source = `see <img src="/uploads/${HEX}/shot.png" alt="a shot"> here`;
    expect(claimed(lex(source, fullStack(GITLAB_MR)))).toEqual([
      {
        type: 'image',
        raw: `<img src="/uploads/${HEX}/shot.png" alt="a shot">`,
        text: 'a shot',
        title: null,
        href: `/uploads/${HEX}/shot.png`,
      },
    ]);
  });

  // The shape GitHub actually writes into a PR body. A lone `<video>` line
  // would be an html block the sanitizer drops; a lone `<img>` line is
  // GitHub's paragraph (embeddedHtml.ts `imageLines`), claimed inline.
  it.each([
    [GITHUB_PR, '<img src="https://github.com/user-attachments/assets/abc">'],
    [GITHUB_PR, '<video src="https://github.com/user-attachments/assets/abc" controls></video>'],
    [GITLAB_MR, `<video src="/uploads/${HEX}/clip.mp4" controls></video>`],
  ])('claims a lone embed line: %s %s', (pr, source) => {
    const tokens = lex(`${source}\n`, fullStack(pr));
    expect(tokens.map((t) => t.type)).toEqual(['paragraph']);
    expect(claimed(tokens).map((t) => t.raw)).toEqual([source]);
  });

  // GitLab keeps a lone `<img>` line an HTML block; the sanitizer's
  // `claimMedia` hands its upload back (ChatMarkdown.forgeAttachmentsHtml).
  it('leaves a lone GitLab upload image line to the HTML block', () => {
    const tokens = lex(`<img src="/uploads/${HEX}/shot.png">\n`, fullStack(GITLAB_MR));
    expect(tokens.map((t) => t.type)).toEqual(['html']);
  });

  it('leaves a lone embed line the forge does not serve as html', () => {
    const tokens = lex('<img src="https://example.test/x.png">\n', fullStack(GITLAB_MR));
    expect(tokens.map((t) => t.type)).toEqual(['html']);
  });

  it.each([
    ['claims a <video> and consumes through its closing tag', `<video src="/uploads/${HEX}/clip.mp4" controls></video>`],
    ['reads the first <source> when the video element carries no src', `<video controls><source src="/uploads/${HEX}/clip.mp4" type="video/mp4"></video>`],
    ['consumes only the open tag of an unpaired <video>', `<video src="/uploads/${HEX}/clip.mp4">`],
  ])('%s', (_name, source) => {
    const [token] = claimed(lex(`x ${source}`, fullStack(GITLAB_MR)));
    expect(token.raw).toBe(source);
    expect(token.href).toBe(`/uploads/${HEX}/clip.mp4`);
  });

  it('leaves a video the forge does not serve to the sanitizer', () => {
    const source = 'x <video src="https://example.test/clip.mp4"></video>';
    expect(claimed(lex(source, fullStack(GITLAB_MR)))).toEqual([]);
  });

  it('claims nothing from HTML on a surface that has HTML disabled', () => {
    const source = `see <img src="/uploads/${HEX}/shot.png">`;
    expect(claimed(lex(source, fullStack(GITLAB_MR, '', false)))).toEqual([]);
  });

  it('claims an <a> to an upload as the Markdown link to it is claimed', () => {
    const source = `x <a href="/uploads/${HEX}/report.pdf">report</a>`;
    expect(claimed(lex(source, fullStack(GITLAB_MR)))).toEqual([
      { type: 'link', raw: `<a href="/uploads/${HEX}/report.pdf">report</a>`, text: 'report', title: 'Download report.pdf', href: `/uploads/${HEX}/report.pdf` },
    ]);
  });
});

// A theme-specific image: each forge-shaped src of a <picture> is claimed,
// and a claim keeps the scheme its authored fragment shows it in.
describe('theme-specific images', () => {
  const UUID_DARK = 'https://github.com/user-attachments/assets/0d0d0d0d-1111-2222-3333-444455556666';
  const UUID_LIGHT = 'https://github.com/user-attachments/assets/1e1e1e1e-1111-2222-3333-444455556666';
  const image = (tokens: readonly Token[]) => {
    const found: Record<string, unknown>[] = [];
    const walk = (list: readonly Token[]) => {
      for (const token of list as unknown as { type: string; tokens?: Token[] }[]) {
        if (token.type === 'image') found.push(token as unknown as Record<string, unknown>);
        if (token.tokens) walk(token.tokens);
      }
    };
    walk(tokens);
    return found;
  };
  const sourceHref = (value: unknown) => parseForgeAttachmentHref(String(value))?.href ?? value;

  it('claims a picture whose source alone is forge-hosted, rewriting only that source', () => {
    const source =
      `x <picture><source media="(prefers-color-scheme: dark)" srcset="${UUID_DARK}">` +
      '<source media="(prefers-color-scheme: light)" srcset="https://example.invalid/light.png">' +
      '<img alt="logo" src="https://example.invalid/default.png" width="40" align="right"></picture>';
    const [token] = image(lex(source, fullStack(GITHUB_PR)));
    const sources = token.sources as Record<string, string>;
    expect(token.href).toBe('https://example.invalid/default.png');
    expect(sourceHref(sources.dark)).toBe(UUID_DARK);
    expect(sources.dark).not.toBe(UUID_DARK);
    expect(sources.light).toBe('https://example.invalid/light.png');
    expect(token).toMatchObject({ width: '40', align: 'right', text: 'logo' });
  });

  it('claims both the fallback and a source the forge serves', () => {
    const source =
      `<picture><source media="(prefers-color-scheme: light)" srcset="${UUID_LIGHT}">` +
      `<img alt="logo" src="${UUID_DARK}"></picture>\n`;
    const [token] = image(lex(source, fullStack(GITHUB_PR)));
    expect(sourceHref(token.href)).toBe(UUID_DARK);
    expect(sourceHref((token.sources as Record<string, string>).light)).toBe(UUID_LIGHT);
  });

  it('leaves a picture nothing of which the forge serves to the embedded-HTML extension', () => {
    const source =
      'x <picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png">' +
      '<img src="https://example.invalid/default.png"></picture>';
    expect(claimed(lex(source, fullStack(GITHUB_PR)))).toEqual([]);
  });

  it.each([
    ['a Markdown image', `![logo](${UUID_DARK}#gh-dark-mode-only)`],
    ['an <img>', `x <img alt="logo" src="${UUID_DARK}#gh-dark-mode-only">`],
  ])('keeps the scheme of %s claimed with a #gh-*-mode-only fragment', (_name, source) => {
    const [token] = image(lex(source, fullStack(GITHUB_PR)));
    expect(sourceHref(token.href)).toBe(`${UUID_DARK}#gh-dark-mode-only`);
    expect(token.shownIn).toBe('dark');
  });

  it('keeps a Markdown label for the plain alt', () => {
    const [token] = image(lex(`![a *b*](${UUID_DARK})`, fullStack(GITHUB_PR)));
    expect(token.text).toBe('a *b*');
    expect((token.tokens as Token[]).map((t) => t.type)).toEqual(['text', 'em']);
  });
});

// Link text is lexed with `inLink` set. A picture there is the link's
// content, as a linked badge or screenshot is, so it is claimed and loaded
// with the user's login; nothing else is.
describe('inside link text', () => {
  const PRIVATE = 'https://private-user-images.githubusercontent.com/123/asset.png?jwt=example';
  const ASSET = 'https://github.com/user-attachments/assets/4f0b0b1e-1111-2222-3333-444455556666';

  // Each link that is not itself a claim, with the claims inside it.
  function linked(tokens: readonly Token[]): { link: string; claims: Claimed[] }[] {
    const out: { link: string; claims: Claimed[] }[] = [];
    const walk = (list: readonly Token[]): void => {
      for (const token of list) {
        const t = token as { type: string; href?: string; tokens?: Token[] };
        if (t.type === 'link' && parseForgeAttachmentHref(t.href ?? '') === null) {
          out.push({ link: t.href ?? '', claims: claimed(t.tokens ?? []) });
        } else if (Array.isArray(t.tokens)) walk(t.tokens);
      }
    };
    walk(tokens);
    return out;
  }

  it.each([
    ['a <picture>', `<picture><img src="${PRIVATE}"></picture>`],
    ['an <img>', `<img src="${PRIVATE}" alt="shot">`],
    ['a markdown image', `![shot](${PRIVATE})`],
  ])('claims %s', (_name, inner) => {
    const tokens = lex(`[${inner}](https://example.com)`, fullStack(GITHUB_PR));
    const links = linked(tokens);
    expect(links.map((each) => each.link)).toEqual(['https://example.com']);
    expect(links[0].claims.map((each) => [each.type, each.href, each.raw])).toEqual([['image', PRIVATE, inner]]);
    // The host renders no control inside the author's link (R1), so the
    // claim says where it is; a claim outside a link says nothing.
    expect(inLinkOf(tokens)).toEqual([true]);
    expect(inLinkOf(lex(`see ${inner} here`, fullStack(GITHUB_PR)))).toEqual([false]);
  });

  it('claims a gitlab upload image', () => {
    const inner = `![shot](/uploads/${HEX}/shot.png)`;
    const [link] = linked(lex(`[${inner}](https://example.com)`, fullStack(GITLAB_MR)));
    expect(link.claims.map((each) => each.raw)).toEqual([inner]);
  });

  it('marks an <a href> around an <img> claimed in link text as inLink', () => {
    expect(inLinkOf(lex(`see <a href="https://example.com/x"><img src="${PRIVATE}"></a> here`, fullStack(GITHUB_PR)))).toEqual([true]);
    expect(inLinkOf(lex(`see <img src="${PRIVATE}"> here`, fullStack(GITHUB_PR)))).toEqual([false]);
    expect(inLinkOf(lex(`${ASSET}`, fullStack(GITHUB_PR)))).toEqual([false]);
  });

  function inLinkOf(tokens: readonly Token[]): boolean[] {
    const out: boolean[] = [];
    const walk = (list: readonly Token[]): void => {
      for (const token of list) {
        const t = token as { type: string; href?: string; inLink?: boolean; tokens?: Token[] };
        if (t.type === 'image' && parseForgeAttachmentHref(t.href ?? '')) out.push(t.inLink === true);
        if (Array.isArray(t.tokens)) walk(t.tokens);
      }
    };
    walk(tokens);
    return out;
  }

  it('claims no player or bare URL', () => {
    expect(claimed(lex(`[<video src="${ASSET}"></video>](https://example.com)`, fullStack(GITHUB_PR)))).toEqual([]);
    expect(claimed(lex(`[${ASSET}](https://example.com)`, fullStack(GITHUB_PR)))).toEqual([]);
  });

  it('stays in the link after an image it claimed', () => {
    // The link tokenizer a claim delegates to clears `inLink`; a player
    // after the image is still inside the link text.
    const source = `[![a](/uploads/${HEX}/a.png) and <video src="/uploads/${HEX}/clip.mp4"></video>](https://example.com)`;
    const [link] = linked(lex(source, fullStack(GITLAB_MR)));
    expect(link.claims.map((each) => each.href)).toEqual([`/uploads/${HEX}/a.png`]);
  });
});
