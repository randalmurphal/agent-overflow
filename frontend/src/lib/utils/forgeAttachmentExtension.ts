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
//
// A `<picture>` is claimed when its fallback or a color scheme's source is
// forge-shaped; each forge-shaped src is rewritten and the rest are left for
// the renderer's URL gate, which picks among them by the reader's scheme.
// A claim keeps its authored URL's `#gh-*-mode-only` as `shownIn`, since the
// rewritten href no longer ends in it.
//
// An image claimed inside link text carries `inLink: true`
// (markdownImageSource.ts#claimedInLink), so the host keeps the author's
// link the one control whatever the bytes turn out to be. GitLab's
// `{width=…}` list after a claim is `markedImageAttributes`'s, as after
// any image.

import type { ColorScheme, ColorSchemeSources, EmbeddedImageToken, Token, Tokens, TokensList } from '../markdown';
import { colorSchemeOnly, markedEmbeddedInlineHtml } from '../markdown';
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
  /** An embedded `<img>`'s authored size and placement (EmbeddedImageToken). */
  width?: string | null;
  height?: string | null;
  align?: string | null;
  hspace?: string | null;
  /** A `<picture>`'s per-scheme sources, forge-shaped ones rewritten. */
  sources?: ColorSchemeSources;
  /** The scheme the authored URL's `#gh-*-mode-only` shows it in. */
  shownIn?: ColorScheme;
  /** Claimed inside link text (markdownImageSource.ts#claimedInLink). */
  inLink?: true;
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

// An image as the embedded-HTML tokenizer reads one: a bare `<img>`, or
// the `<picture>` whose fallback it is.
const IMG_OPEN = /^<(?:img|picture)\b/i;
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

  // `label` is a Markdown label's inline tokens, from which the renderer
  // takes the plain alt (render/imageAlt.ts); none for an attribute's alt.
  const imageToken = (raw: string, href: string, alt: string, inLink = false, label: Token[] = []): GenericImageToken => {
    const shownIn = colorSchemeOnly(href);
    return {
      type: 'image',
      raw,
      href: rewrite(href),
      title: browserUrlForForgeAttachment(forge, href, webBase, pr),
      text: alt || forgeAttachmentName(forge, href),
      tokens: alt ? label : [],
      ...(inLink ? { inLink: true } : {}),
      ...(shownIn ? { shownIn } : {}),
    };
  };

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
      // Inside link text only a picture is claimed, as a linked badge or
      // screenshot is: a link holds no link, a player is interactive
      // content no anchor may hold, and a bare URL there is the link's
      // label, which marked's own `url` rule leaves as text too.
      const inLink = ctx.lexer?.state?.inLink === true;

      if (first === 91 || first === 33) {
        const image = src.startsWith('![');
        if (!image && (inLink || first !== 91)) return undefined;
        return markdownToken(src, ctx, inLink);
      }
      if (first === 60) {
        if (!embeddedHtml) return undefined;
        if (IMG_OPEN.test(src)) return embeddedImageToken(src, ctx, inLink);
        if (!inLink && VIDEO_OPEN.test(src)) return videoToken(src);
        return undefined;
      }
      if (inLink) return undefined;
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
      if (IMG_OPEN.test(src)) embed = embeddedImageToken(src, ctx, false);
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
    inLink: boolean,
  ): GenericLinkToken | GenericImageToken | undefined {
    const token = ctx.lexer?.tokenizer?.link?.(src);
    if (!token || (token.type !== 'link' && token.type !== 'image')) return undefined;
    const href = typeof token.href === 'string' ? token.href : '';
    if (!isForgeAttachmentHref(forge, href)) return undefined;
    if (token.type === 'image') return imageToken(token.raw, href, token.text ?? '', inLink, token.tokens);
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
    inLink: boolean,
  ): GenericImageToken | undefined {
    // Delegate to the embedded-HTML inline tokenizer so `<img>` attribute
    // parsing has one implementation. It answers an image token or nothing.
    const token = markedEmbeddedInlineHtml.tokenizer!.call(
      ctx as never,
      src,
      [],
    ) as EmbeddedImageToken | undefined;
    if (!token || token.type !== 'image') return undefined;
    const href = typeof token.href === 'string' ? token.href : '';
    const claimed = isForgeAttachmentHref(forge, href);
    let sources: ColorSchemeSources | undefined;
    let claimsSource = false;
    if (token.sources) {
      sources = {};
      for (const scheme of ['dark', 'light'] as const) {
        const src = token.sources[scheme];
        if (src === undefined) continue;
        const forgeShaped = isForgeAttachmentHref(forge, src);
        claimsSource ||= forgeShaped;
        sources[scheme] = forgeShaped ? rewrite(src) : src;
      }
    }
    if (!claimed && !claimsSource) return undefined;
    // The authored size and placement go with the claim (render/imageSize.ts).
    const base = claimed
      ? imageToken(token.raw, href, token.text ?? '', inLink)
      : { ...token, tokens: [], ...(inLink ? { inLink: true as const } : {}) };
    return {
      ...base,
      width: token.width,
      height: token.height,
      align: token.align,
      hspace: token.hspace,
      ...(sources ? { sources } : {}),
    };
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
