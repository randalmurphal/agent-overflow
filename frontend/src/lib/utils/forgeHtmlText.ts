// What a forge does with the text of the HTML in a PR/MR body or comment,
// as the embedded HTML sanitizer's hooks (render/htmlSanitize.ts): a forge
// runs its text pipeline over the whole rendered document, so a reference,
// mention or emoji shortcode inside an HTML block gets what it gets in a
// paragraph, the PR's facts included: a text run reads them under its
// block's reading, as a paragraph's lex does under its scope. Built from
// the extensions the surface's Markdown uses, so each rule has one owner:
// the extensions read `state.inHtmlText` for what differs in HTML text. An
// HTML `href` or `src` is decided by the same extensions' destination hooks
// (chatMarkdownOptions.ts).

import { Lexer, type Extension, type SanitizeEmbeddedHtmlOptions } from '../markdown';
import type { ForgeAttachmentSource } from './forgeAttachments';

type ForgeHtmlOptions = Required<Pick<SanitizeEmbeddedHtmlOptions, 'text' | 'anchors'>>;

// `htmlTextTokens` restores the state it sets, so one serves every run.
let lexer: Lexer | undefined;

/**
 * The hooks for `source`'s surface. `extensions` answers the text
 * extensions in their Markdown order when a run is read, so an emoji table
 * that arrives later is read by the next render.
 */
export function forgeHtmlOptions(source: ForgeAttachmentSource, extensions: () => readonly Extension[]): ForgeHtmlOptions {
  return {
    text: (text, linkable, reading) => (lexer ??= new Lexer()).htmlTextTokens(text, extensions(), linkable, reading),
    // GitHub keeps every element's `id` as a jump target; GitLab only `<a name>`.
    anchors: { ids: source.pr.forge === 'github' },
  };
}
