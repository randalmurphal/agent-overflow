// How ChatMarkdown parses and sanitizes a body, built in one place so the
// renderer and find's projection of it (markdownFindText.ts) read the same
// text: the parser extensions in their order, and the options the embedded
// HTML sanitizer runs with. A surface that renders one PR's content
// (FORGE_ATTACHMENT_SOURCE_CONTEXT) claims forge-hosted media as tokens
// (`buildForgeAttachmentExtension`) and, inside an HTML block, through the
// sanitizer's `claimMedia`, which drops a claimed video's children.

import type { ClaimEmbeddedMedia, Extension, SanitizeEmbeddedHtmlOptions } from '../../../markdown';
import { EMBEDDED_HTML_EXTENSIONS } from '../../../markdown';
import { buildForgeAttachmentExtension } from '../../../utils/forgeAttachmentExtension';
import { buildForgeAttachmentHref, isForgeAttachmentHref, type ForgeAttachmentSource } from '../../../utils/forgeAttachments';

export interface ChatMarkdownForgeOptions {
  /** The forge attachment extensions, first in the list; none off a PR. */
  readonly extensions: readonly Extension[] | undefined;
  /** What the embedded HTML sanitizer runs with. */
  readonly html: SanitizeEmbeddedHtmlOptions | undefined;
}

/** What one PR's forge attachments add to the parse and the sanitizer;
 * nothing without a GitHub or GitLab source. */
export function chatMarkdownForgeOptions(source: ForgeAttachmentSource | null | undefined, embeddedHtml: boolean): ChatMarkdownForgeOptions {
  if (!source || (source.pr.forge !== 'github' && source.pr.forge !== 'gitlab')) return { extensions: undefined, html: undefined };
  const { pr, backend, webBase } = source;
  const claimMedia: ClaimEmbeddedMedia = (_tag, src) =>
    isForgeAttachmentHref(pr.forge, src) ? buildForgeAttachmentHref({ href: src, pr, backend, webBase }) : null;
  return {
    extensions: buildForgeAttachmentExtension({ pr, backend, webBase, embeddedHtml }),
    html: { claimMedia },
  };
}

/** ChatMarkdown's parser extensions in order. Forge attachments first,
 * then preview: both the path-link extension and the embedded-HTML pair
 * claim starts the forge one needs (`[`, `![`, `<img`, `<video`), and it
 * returns undefined for everything that is not a forge-shaped href, so
 * nothing behind it loses a token. Preview then precedes path links, which
 * claim every remaining `[…](…)`. */
export function chatMarkdownExtensions(parts: {
  forge: readonly Extension[] | undefined;
  preview?: Extension;
  pathLink?: Extension;
  embeddedHtml: boolean;
}): Extension[] {
  const list: Extension[] = [];
  if (parts.forge) list.push(...parts.forge);
  if (parts.preview) list.push(parts.preview);
  if (parts.pathLink) list.push(parts.pathLink);
  if (parts.embeddedHtml) list.push(...EMBEDDED_HTML_EXTENSIONS);
  return list;
}
