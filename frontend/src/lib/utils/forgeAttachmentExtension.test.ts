import { describe, expect, it, vi } from 'vitest';
import { EMBEDDED_HTML_EXTENSIONS, Lexer, type Token } from '../markdown';
import { buildForgeAttachmentExtension } from './forgeAttachmentExtension';
import { parseForgeAttachmentHref } from './forgeAttachments';
import { buildPathLinkExtension } from './pathLinkExtension';
import type { PRRef } from './prReference';

vi.mock('../native/platform', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../native/platform')>()),
  isNativeShell: () => false,
}));

const HEX = '0123456789abcdef0123456789abcdef';
const GITHUB_PR: PRRef = { forge: 'github', namespace: 'acme', repo: 'widget', number: 7 };
const GITLAB_MR: PRRef = { forge: 'gitlab', namespace: 'group/sub', repo: 'widget', number: 3 };
const GITLAB_WEB = 'https://gitlab.example.test/group/sub/widget/-/merge_requests/3';

type Ext = { level: 'inline' | 'block'; tokenizer: unknown; start?: unknown };

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
      block: extensions.filter((e) => e.level === 'block').map((e) => e.tokenizer),
      inline: extensions.filter((e) => e.level === 'inline').map((e) => e.tokenizer),
      childTokens: {},
      renderers: {},
      startBlock: [],
      startInline: extensions.map((e) => e.start).filter(Boolean),
    },
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
  if (embeddedHtml) list.push(...(EMBEDDED_HTML_EXTENSIONS as unknown as Ext[]));
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
    expect(inline.start('text https://github.com/user-attachments/assets/x')).toBe(5);
    expect(inline.start('no attachment here')).toBeUndefined();
    // Glued into a longer word: not a reference, so not a bound.
    expect(inline.start('xhttps://github.com/user-attachments/assets/x')).toBeUndefined();
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

  // The shape GitHub actually writes into a PR body. Marked classifies a
  // lone tag line as an html BLOCK before any inline tokenizer runs, so
  // without the block half the sanitizer would render an <img> pointing at
  // a URL this page cannot read.
  it.each([
    `<img src="/uploads/${HEX}/shot.png">`,
    `<video src="/uploads/${HEX}/clip.mp4" controls></video>`,
  ])('claims a lone embed line: %s', (source) => {
    const tokens = lex(`${source}\n`, fullStack(GITLAB_MR));
    expect(tokens.map((t) => t.type)).toEqual(['paragraph']);
    expect(claimed(tokens).map((t) => t.raw)).toEqual([source]);
  });

  it('leaves a lone embed line the forge does not serve as html', () => {
    const tokens = lex('<img src="https://example.test/x.png">\n', fullStack(GITLAB_MR));
    expect(tokens.map((t) => t.type)).toEqual(['html']);
  });

  it('claims a <video> and consumes through its closing tag', () => {
    const source = `<video src="/uploads/${HEX}/clip.mp4" controls></video>`;
    const [token] = claimed(lex(`x ${source}`, fullStack(GITLAB_MR)));
    expect(token.raw).toBe(source);
    expect(token.href).toBe(`/uploads/${HEX}/clip.mp4`);
  });

  it('reads the first <source> when the video element carries no src', () => {
    const source = `<video controls><source src="/uploads/${HEX}/clip.mp4" type="video/mp4"></video>`;
    const [token] = claimed(lex(`x ${source}`, fullStack(GITLAB_MR)));
    expect(token.raw).toBe(source);
    expect(token.href).toBe(`/uploads/${HEX}/clip.mp4`);
  });

  it('consumes only the open tag of an unpaired <video>', () => {
    const source = `<video src="/uploads/${HEX}/clip.mp4">`;
    const [token] = claimed(lex(`x ${source}`, fullStack(GITLAB_MR)));
    expect(token.raw).toBe(source);
  });

  it('leaves a video the forge does not serve to the sanitizer', () => {
    const source = 'x <video src="https://example.test/clip.mp4"></video>';
    expect(claimed(lex(source, fullStack(GITLAB_MR)))).toEqual([]);
  });

  it('claims nothing from HTML on a surface that has HTML disabled', () => {
    const source = `see <img src="/uploads/${HEX}/shot.png">`;
    expect(claimed(lex(source, fullStack(GITLAB_MR, '', false)))).toEqual([]);
  });

  it('leaves an <a> to the embedded-HTML extension', () => {
    const source = `x <a href="/uploads/${HEX}/report.pdf">report</a>`;
    expect(claimed(lex(source, fullStack(GITLAB_MR)))).toEqual([]);
  });
});
