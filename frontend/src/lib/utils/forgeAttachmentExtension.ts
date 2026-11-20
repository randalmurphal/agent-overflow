// Claims forge-hosted attachments in PR/MR bodies and review comments, and
// rewrites them to the nonce-gated `agent-overflow:forge?…` href the render
// and click paths recognize. A claim is made ONLY for an href that is
// forge-shaped for THIS PR's forge; anything else is left as written.
//
// Most of it is a destination hook (`destination`): the engine and the
// HTML sanitizer run it on every link and image destination however it was
// written, inline, by reference, as an HTML `<a>` or `<img>`, as the URL a
// renderer would fetch (`canonicalHref`). So
//
//   ![alt](/uploads/<hex>/shot.png)   any image syntax   → claimed image
//   [report.pdf](/uploads/<hex>/…)    any link syntax    → download link
//   <picture> with a forge source     its sources        → rewritten
//
// and the tokenizers claim the two shapes that are not a link or image
// destination: a bare GitHub attachment URL, and a `<video>`.
//
// A bare GitHub attachment URL becomes an IMAGE token on purpose: that one
// spelling is how GitHub embeds both videos and pictures, and the host
// renders by the kind the bytes turned out to be, so a video URL becomes a
// player and an image URL a picture — what the forge itself does.
//
// A claim keeps its authored URL's `#gh-*-mode-only` as `shownIn`, since
// the rewritten href no longer ends in it. An image claimed inside link
// text carries `inLink: true` (markdownImageSource.ts#claimedInLink), so
// the host keeps the author's link the one control whatever the bytes turn
// out to be. GitLab's `{width=…}` list after a claim is
// `markedImageAttributes`'s, as after any image.

import type { ColorScheme, ColorSchemeSources, DestinationHook, EmbeddedImageToken, Extension, Tokens } from '../markdown';
import { colorSchemeOnly, readVideo, type Lexer } from '../markdown';
import { fileSaveAction } from './fileSaveAction';
import {
  buildForgeAttachmentHref,
  browserUrlForForgeAttachment,
  FORGE_ATTACHMENT_HREF_PREFIX,
  forgeAttachmentName,
  isForgeAttachmentHref,
  type ForgeAttachmentSource,
} from './forgeAttachments';

type ClaimedImage = Tokens.Image & Partial<Pick<EmbeddedImageToken, 'width' | 'height' | 'align' | 'hspace'>> & {
  /** A `<picture>`'s per-scheme sources, forge-shaped ones rewritten. */
  sources?: ColorSchemeSources;
  /** The scheme the authored URL's `#gh-*-mode-only` shows it in. */
  shownIn?: ColorScheme;
  /** Claimed inside link text (markdownImageSource.ts#claimedInLink). */
  inLink?: true;
};

// The two bare-URL openings GitHub writes. Both are scanned in ONE pass, and
// the match must start the line or follow a boundary character so a URL glued
// into a longer word is not claimed. Case-insensitive because a pasted URL
// routinely arrives with a capitalized host.
const BARE_GITHUB_URL_SCAN =
  /(?:^|[\s(<[{'"`])(https?:\/\/(?:github\.com\/user-attachments\/|private-user-images\.githubusercontent\.com\/))/gi;

/**
 * Build the extensions for one PR-scoped surface. Surfaces with no forge
 * source (agent chat, settings previews) build none and behave exactly as
 * before.
 *
 * Two of them, because a forge writes a `<video>` in two positions. The
 * inline one carries the destination hook and claims bare URLs and an
 * inline `<video>`; the BLOCK one claims a `<video …></video>` alone on its
 * line, which the sanitizer, having no `<video>` element, would drop.
 */
export function buildForgeAttachmentExtension(opts: ForgeAttachmentSource & { embeddedHtml: boolean }): Extension[] {
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
  // takes the plain alt (render/imageAlt.ts). The authored title is kept;
  // without one the image is titled with its URL on the forge.
  const claimedImage = (raw: string, href: string, alt: string, title: string | null, inLink: boolean, label: Tokens.Image['tokens']): ClaimedImage => {
    const shownIn = colorSchemeOnly(href);
    return {
      type: 'image',
      raw,
      href: rewrite(href),
      title: title || browserUrlForForgeAttachment(forge, href, webBase, pr),
      text: alt || forgeAttachmentName(forge, href),
      tokens: alt ? label : [],
      ...(inLink ? { inLink: true } : {}),
      ...(shownIn ? { shownIn } : {}),
    };
  };

  // Inside link text only a picture is claimed, as a linked badge or
  // screenshot is: a link holds no link.
  const destination: DestinationHook = (token, { inLink }) => {
    if (token.type === 'link') {
      if (inLink || !isForgeAttachmentHref(pr, token.href)) return undefined;
      return { ...token, href: rewrite(token.href), title: linkTitle(token.href) };
    }
    const image = token as ClaimedImage;
    const claimed = isForgeAttachmentHref(pr, image.href);
    // A `<picture>` is claimed when a color scheme's source is forge-shaped
    // too; the other sources are left for the renderer's URL gate.
    let sources: ColorSchemeSources | undefined;
    let claimsSource = false;
    if (image.sources) {
      sources = {};
      for (const scheme of ['dark', 'light'] as const) {
        const src = image.sources[scheme];
        if (src === undefined) continue;
        const forgeShaped = isForgeAttachmentHref(pr, src);
        claimsSource ||= forgeShaped;
        sources[scheme] = forgeShaped ? rewrite(src) : src;
      }
    }
    if (!claimed && !claimsSource) return undefined;
    // The authored size and placement go with the claim (render/imageSize.ts).
    return {
      ...image,
      ...(claimed ? claimedImage(image.raw, image.href, image.text ?? '', image.title, inLink, image.tokens) : inLink ? { inLink: true as const } : {}),
      ...(sources ? { sources } : {}),
    };
  };

  const inline: Extension = {
    name: 'forgeAttachment',
    level: 'inline',
    destination,
    start(src) {
      // The engine's inline text rule already stops at `<`, so a video
      // reaches the tokenizer without help. A bare URL is the one shape
      // that needs a bound, and one scan answers both hosts.
      BARE_GITHUB_URL_SCAN.lastIndex = 0;
      const match = BARE_GITHUB_URL_SCAN.exec(src);
      return match ? match.index + match[0].length - match[1].length : undefined;
    },
    tokenizer(src) {
      const first = src.charCodeAt(0);
      // Opening-character check before any parsing: the tokenizer runs at
      // every candidate position in every block.
      if (first !== 60 /* < */ && first !== 104 /* h */ && first !== 72 /* H */) return undefined;
      // Inside link text a player is interactive content no anchor may
      // hold, and a bare URL there is the link's label, which the engine's
      // own `url` rule leaves as text too.
      if (this.lexer.state.inLink) return undefined;
      if (first === 60) return embeddedHtml ? videoToken(this.lexer, src) : undefined;
      // GitLab surfaces never claim a github.com URL: the fetch would use the
      // wrong CLI, and a `glab` login says nothing about github.com.
      if (forge !== 'github') return undefined;
      // The engine's own autolink boundary, so a bare attachment URL ends
      // exactly where the engine would have ended it.
      const url = this.lexer.tokenizer.url(src);
      if (!url || !isForgeAttachmentHref(pr, url.raw)) return undefined;
      return claimedImage(url.raw, url.raw, '', null, false, []);
    },
  };

  if (!embeddedHtml) return [inline];

  const block: Extension = {
    name: 'forgeAttachmentBlock',
    level: 'block',
    tokenizer(src) {
      if (src.charCodeAt(0) !== 60 /* < */) return undefined;
      const embed = videoToken(this.lexer, src);
      if (!embed) return undefined;
      // Only a LONE tag is a block. Anything else on the line makes it a
      // paragraph, and the inline tokenizer owns that case.
      const trailing = /^[ \t]*(?:\r?\n|$)/.exec(src.slice(embed.raw.length));
      if (!trailing) return undefined;
      const paragraph: Tokens.Paragraph = {
        type: 'paragraph',
        raw: src.slice(0, embed.raw.length + trailing[0].length),
        text: embed.raw,
        tokens: [embed],
      };
      return paragraph;
    },
  };
  return [inline, block];

  /**
   * A `<video>`, read as the sanitizer reads one (`readVideo`), as the image
   * of its first src the destination rule claims. An unpaired `<video>` is
   * its open tag alone; the sanitizer keeps whatever follows.
   */
  function videoToken(lexer: Lexer, src: string): ClaimedImage | undefined {
    const video = readVideo(src);
    if (!video) return undefined;
    for (const href of video.srcs) {
      const decided = lexer.tokenizer.destination({ type: 'image', raw: video.raw, href, title: null, text: video.title ?? '', tokens: [] });
      if (decided.href.startsWith(FORGE_ATTACHMENT_HREF_PREFIX)) return decided;
    }
    return undefined;
  }
}
