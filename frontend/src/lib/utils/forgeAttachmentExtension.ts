// Marked inline extension that claims forge-hosted attachment references in
// PR/MR bodies and review comments, and rewrites them to the nonce-gated
// `agent-overflow:forge?…` href the render and click paths recognize.
//
// Modelled on `pathLinkExtension.ts`: it runs at every inline position, and
// it claims a token ONLY when the resolved href is forge-shaped for THIS
// PR's forge. Anything else returns undefined, so the path-link extension,
// the embedded-HTML extensions and marked's built-ins see the source exactly
// as written. It is pushed FIRST in ChatMarkdown's extension list, because
// the later ones claim overlapping starts (`[`, `<`) and would otherwise
// take these tokens first.
//
// Four shapes, matching what the two forges actually emit:
//
//   ![alt](/uploads/<hex>/shot.png)   markdown image  → image token
//   [report.pdf](/uploads/<hex>/…)    markdown link   → link token (download)
//   https://github.com/user-attachments/assets/<uuid>   bare URL → image token
//   <img src=…> and <video …>         embedded HTML   → image token
//
// A bare GitHub attachment URL becomes an IMAGE token on purpose: that one
// spelling is how GitHub embeds both videos and pictures, and the host
// renders by the kind the bytes turned out to be, so a video URL becomes a
// player and an image URL a picture — what the forge itself does.

import type { Token, Tokens, TokensList } from '../markdown';
import { markedEmbeddedInlineHtml } from '../markdown';
import { fileSaveAction } from './fileSaveAction';
import {
  buildForgeAttachmentHref,
  browserUrlForForgeAttachment,
  forgeAttachmentName,
  isForgeAttachmentHref,
  type ForgeAttachmentSource,
} from './forgeAttachments';

interface ForgeAttachmentInlineExtension {
  name: 'forgeAttachment';
  level: 'inline';
  start(src: string): number | undefined;
  tokenizer(
    this: unknown,
    src: string,
    tokens: Token[] | TokensList,
  ): GenericLinkToken | GenericImageToken | undefined;
}

interface ForgeAttachmentBlockExtension {
  name: 'forgeAttachmentBlock';
  level: 'block';
  tokenizer(this: unknown, src: string, tokens: Token[] | TokensList): ParagraphToken | undefined;
}

/** A one-element paragraph wrapping a claimed block-level embed. */
interface ParagraphToken extends Tokens.Paragraph {
  type: 'paragraph';
}

interface GenericLinkToken extends Tokens.Link {
  type: 'link';
}

interface GenericImageToken extends Tokens.Image {
  type: 'image';
}

interface ForgeTokenizerContext {
  lexer?: {
    state?: { inLink?: boolean };
    tokenizer?: {
      link?: (src: string) => Tokens.Link | Tokens.Image | undefined;
      url?: (src: string) => Tokens.Link | undefined;
    };
  };
}

// The two bare-URL openings GitHub writes. Both are scanned in ONE pass, and
// the match must start the line or follow a boundary character so a URL glued
// into a longer word is not claimed. Case-insensitive because a pasted URL
// routinely arrives with a capitalized host.
const BARE_GITHUB_URL_SCAN =
  /(?:^|[\s(<[{'"`])(https?:\/\/(?:github\.com\/user-attachments\/|private-user-images\.githubusercontent\.com\/))/gi;
// Fallback URL run for an engine whose `url()` tokenizer is a no-op (gfm
// off). Same boundary GFM autolinks use, minus the trailing punctuation a
// sentence leaves behind.
const BARE_URL_RUN = /^https?:\/\/[^\s<>)]+/i;
const TRAILING_PUNCTUATION = /[.,;:!?]+$/;

const IMG_OPEN = /^<img\b/i;
const VIDEO_OPEN = /^<video\b((?:[^>"'\n]|"[^"]*"|'[^']*')*)>/i;
const VIDEO_CLOSE = /<\/video\s*>/i;
const SOURCE_TAG = /<source\b((?:[^>"'\n]|"[^"]*"|'[^']*')*)>/i;

function attrOf(attrs: string, name: string): string | null {
  const match = new RegExp(
    `(?:^|\\s)${name}\\s*=\\s*("([^"]*)"|'([^']*)'|([^\\s>]+))`,
    'i',
  ).exec(attrs);
  if (!match) return null;
  return match[2] ?? match[3] ?? match[4] ?? '';
}

/**
 * Build the extensions for one PR-scoped surface. Surfaces with no forge
 * source (agent chat, settings previews) build none and behave exactly as
 * before.
 *
 * Two of them, because a forge writes an embed in two positions. Inline is
 * the general case. The BLOCK one exists for the single most common shape
 * GitHub produces — a bare `<img …>` or `<video …></video>` alone on its
 * line — which marked classifies as an html BLOCK before any inline
 * tokenizer runs, and which the sanitizer would otherwise render as an
 * `<img>` pointing at a URL this page cannot read.
 */
export function buildForgeAttachmentExtension(
  opts: ForgeAttachmentSource & { embeddedHtml: boolean },
): [ForgeAttachmentInlineExtension, ...ForgeAttachmentBlockExtension[]] {
  const { pr, backend, webBase, embeddedHtml } = opts;
  const forge = pr.forge;

  const rewrite = (href: string): string =>
    buildForgeAttachmentHref({ href, pr, backend, webBase });

  // The verb is the click's own decision (`fileSaveAction`), so the
  // tooltip never promises a download that turns out to open the forge.
  const linkTitle = (href: string): string => {
    const name = forgeAttachmentName(forge, href) || 'attachment';
    const action = fileSaveAction(
      backend,
      browserUrlForForgeAttachment(forge, href, webBase, pr),
    );
    return `${action === 'open-externally' ? 'Open' : 'Download'} ${name}`;
  };

  const imageToken = (raw: string, href: string, alt: string): GenericImageToken => ({
    type: 'image',
    raw,
    href: rewrite(href),
    title: browserUrlForForgeAttachment(forge, href, webBase, pr),
    text: alt || forgeAttachmentName(forge, href),
    tokens: [],
  });

  const inline: ForgeAttachmentInlineExtension = {
    name: 'forgeAttachment',
    level: 'inline',
    start(src) {
      // Marked's inline text rule already stops unconditionally at `[` and
      // `<`, so those positions reach the tokenizer without help. A bare URL
      // is the one shape that needs a bound, and one scan answers both hosts.
      BARE_GITHUB_URL_SCAN.lastIndex = 0;
      const match = BARE_GITHUB_URL_SCAN.exec(src);
      return match ? match.index + match[0].length - match[1].length : undefined;
    },
    tokenizer(this: unknown, src) {
      const first = src.charCodeAt(0);
      // Opening-character check before any parsing: the tokenizer runs at
      // every candidate position in every block.
      if (first !== 91 /* [ */ && first !== 33 /* ! */ && first !== 60 /* < */
        && first !== 104 /* h */ && first !== 72 /* H */) return undefined;
      const ctx = this as ForgeTokenizerContext;
      if (ctx.lexer?.state?.inLink === true) return undefined;

      if (first === 91 || first === 33) {
        if (!src.startsWith('[') && !src.startsWith('![')) return undefined;
        return markdownToken(src, ctx);
      }
      if (first === 60) {
        if (!embeddedHtml) return undefined;
        if (IMG_OPEN.test(src)) return embeddedImageToken(src, ctx);
        if (VIDEO_OPEN.test(src)) return videoToken(src);
        return undefined;
      }
      return bareUrlToken(src, ctx);
    },
  };

  if (!embeddedHtml) return [inline];

  const block: ForgeAttachmentBlockExtension = {
    name: 'forgeAttachmentBlock',
    level: 'block',
    tokenizer(this: unknown, src) {
      if (src.charCodeAt(0) !== 60 /* < */) return undefined;
      const ctx = this as ForgeTokenizerContext;
      let embed: GenericImageToken | undefined;
      if (IMG_OPEN.test(src)) embed = embeddedImageToken(src, ctx);
      else if (VIDEO_OPEN.test(src)) embed = videoToken(src);
      if (!embed) return undefined;
      // Only a LONE tag is a block. Anything else on the line makes it a
      // paragraph, and the inline tokenizer owns that case.
      const trailing = /^[ \t]*(?:\r?\n|$)/.exec(src.slice(embed.raw.length));
      if (!trailing) return undefined;
      return {
        type: 'paragraph',
        raw: src.slice(0, embed.raw.length + trailing[0].length),
        text: embed.raw,
        tokens: [embed],
      };
    },
  };
  return [inline, block];

  function markdownToken(
    src: string,
    ctx: ForgeTokenizerContext,
  ): GenericLinkToken | GenericImageToken | undefined {
    const token = ctx.lexer?.tokenizer?.link?.(src);
    if (!token || (token.type !== 'link' && token.type !== 'image')) return undefined;
    const href = typeof token.href === 'string' ? token.href : '';
    if (!isForgeAttachmentHref(forge, href)) return undefined;
    if (token.type === 'image') {
      return imageToken(token.raw, href, token.text ?? '');
    }
    return {
      ...token,
      type: 'link',
      href: rewrite(href),
      title: linkTitle(href),
    } as GenericLinkToken;
  }

  function embeddedImageToken(
    src: string,
    ctx: ForgeTokenizerContext,
  ): GenericImageToken | undefined {
    // Delegate to the embedded-HTML inline tokenizer so `<img>` attribute
    // parsing has one implementation. It answers an image token or nothing.
    const token = markedEmbeddedInlineHtml.tokenizer.call(
      ctx as never,
      src,
      [],
    ) as Tokens.Image | undefined;
    if (!token || token.type !== 'image') return undefined;
    const href = typeof token.href === 'string' ? token.href : '';
    if (!isForgeAttachmentHref(forge, href)) return undefined;
    return imageToken(token.raw, href, token.text ?? '');
  }

  function videoToken(src: string): GenericImageToken | undefined {
    const open = VIDEO_OPEN.exec(src);
    if (!open) return undefined;
    const rest = src.slice(open[0].length);
    const close = VIDEO_CLOSE.exec(rest);
    // A self-closing or unpaired `<video>` consumes only its open tag; the
    // sanitizer keeps its current behaviour for whatever follows.
    const raw = close
      ? src.slice(0, open[0].length + close.index + close[0].length)
      : open[0];
    const attrs = open[1] ?? '';
    let href = attrOf(attrs, 'src');
    if (href === null && close) {
      const source = SOURCE_TAG.exec(rest.slice(0, close.index));
      if (source) href = attrOf(source[1] ?? '', 'src');
    }
    if (href === null || !isForgeAttachmentHref(forge, href)) return undefined;
    return imageToken(raw, href, attrOf(attrs, 'title') ?? '');
  }

  function bareUrlToken(
    src: string,
    ctx: ForgeTokenizerContext,
  ): GenericImageToken | undefined {
    // GitLab surfaces never claim a github.com URL: the fetch would use the
    // wrong CLI, and a `glab` login says nothing about github.com.
    if (forge !== 'github') return undefined;
    // The engine's own autolink boundary when it has one (gfm), so a bare
    // attachment URL ends exactly where marked would have ended it.
    const engineToken = ctx.lexer?.tokenizer?.url?.(src);
    let raw = engineToken?.raw ?? '';
    if (raw === '') {
      const run = BARE_URL_RUN.exec(src);
      if (!run) return undefined;
      raw = run[0].replace(TRAILING_PUNCTUATION, '');
      if (raw === '') return undefined;
    }
    if (!isForgeAttachmentHref('github', raw)) return undefined;
    return imageToken(raw, raw, '');
  }
}
