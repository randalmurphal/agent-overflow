// What a forge does with the text and links of the HTML in a PR/MR body or
// comment, as the embedded HTML sanitizer's hooks (render/htmlSanitize.ts):
// a forge runs its text pipeline over the whole rendered document, so a
// reference, mention, emoji shortcode or relative link inside an HTML block
// gets what it gets in a paragraph. Built from the extensions and resolver
// the surface's Markdown uses, so each rule has one owner: the extensions
// read `state.inHtmlText` for what differs in HTML text.

import { Lexer, type Extension, type SanitizeEmbeddedHtmlOptions } from '../markdown';
import { browserUrlForForgeAttachment, isForgeAttachmentHref, type ForgeAttachmentSource } from './forgeAttachments';
import type { ForgeReferences } from './forgeReferenceExtension';

type ForgeHtmlOptions = Required<Pick<SanitizeEmbeddedHtmlOptions, 'text' | 'href' | 'anchors'>>;

// `htmlTextTokens` restores the state it sets, so one serves every run.
let lexer: Lexer | undefined;

/**
 * The hooks for `source`'s surface. `extensions` answers the text
 * extensions in their Markdown order when a run is read, so an emoji table
 * that arrives later is read by the next render.
 */
export function forgeHtmlOptions(
  source: ForgeAttachmentSource,
  references: ForgeReferences,
  extensions: () => readonly Extension[],
): ForgeHtmlOptions {
  const { pr, webBase } = source;
  return {
    text: (text, linkable) => (lexer ??= new Lexer()).htmlTextTokens(text, extensions(), linkable),
    // An upload link opens where the forge serves it, as a paragraph's
    // attachment link falls back to.
    href: (href) =>
      isForgeAttachmentHref(pr.forge, href) ? browserUrlForForgeAttachment(pr.forge, href, webBase, pr) : references.resolveHref(href),
    // GitHub keeps every element's `id` as a jump target; GitLab only `<a name>`.
    anchors: { ids: pr.forge === 'github' },
  };
}
