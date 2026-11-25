/**
 * The markdown renderer's public surface.
 *
 * Exactly what the app imports and nothing more: the barrel is the seam
 * between `chat/`'s hosts and this tree, so a symbol reaching it is a
 * deliberate widening. Everything else stays module-local — reach into
 * `parser/` or `render/` directly only from inside this directory.
 */
export { default as Streamdown } from './render/Streamdown.svelte';
export { useStreamdown } from './render/context.svelte';
export type { Theme } from './render/theme';
export type { Footnote } from './parser/extensions/footnotes';
export {
  Lexer,
  lex,
  lexFootnoteDefinitions,
  parseBlocks,
  createParseBlocksCache,
  updateParseBlockStringMaterialization,
  incrementalLex,
  createIncrementalLexCache,
  createProvenAppend,
  createMaterializedProvenAppend,
  matchesProvenAppend,
  alignmentAtEnd,
  blockAlignment,
  topLevelShape,
  fenceLanguage,
} from './parser/index';
export type {
  Alignment,
  Extension,
  LexerOptions,
  Token,
  Tokens,
  TokensList,
  StreamdownToken,
  ParseBlocksCache,
  ParseBlocksLexPath,
  IncrementalLexCache,
  ProvenAppend,
} from './parser/index';
export { parseIncompleteMarkdown } from './parser/incompleteMarkdown';
export { EMBEDDED_HTML_EXTENSIONS, embeddedHtmlFor, htmlAttributeValue, readVideo, type EmbeddedHtml } from './parser/extensions/embeddedHtml';
// How the parse reads a destination as written, for a scan of raw text.
export { resolveLinkText } from './parser/extensions/htmlEntities';
export type { HtmlPolicy } from './parser/extensions/htmlPolicy';
export type { EmbeddedImageToken } from './parser/extensions/embeddedHtml';
// GitHub's soft-break-as-<br> rendering, for a surface showing GitHub content.
export { markedSoftBreak } from './parser/extensions/softBreak';
// GitLab's image attribute list (`{width=… height=…}`), for a surface
// showing GitLab content. It sizes the image token before it, whichever
// extension claimed that image.
export { markedImageAttributes } from './parser/extensions/imageAttributes';
// A forge surface leaves agent chat's own grammar out (`[center]`,
// citations, MDX, `~sub~`/`^sup^`, `:term:` lists, chat's inline math).
export { markedForgeGrammar } from './parser/index';
// Forge syntax: GFM's `~text~` strikethrough; color chips in code spans
// (GitHub's forms, then GitLab's CSS forms); and GitLab's inline diff,
// `>>>` quotes, front matter, `[~]` and bare task boxes, task boxes in
// table cells, description lists and `json:table` fences.
export { markedStrikethrough } from './parser/extensions/strikethrough';
export { markedColorChips, markedCssColorChips } from './parser/extensions/colorChips';
export { markedInlineDiff } from './parser/extensions/inlineDiff';
export { markedMultilineBlockquote } from './parser/extensions/multilineBlockquote';
export { markedFrontMatter } from './parser/extensions/frontMatter';
export { markedInapplicableTaskList } from './parser/extensions/list';
// Each forge's list numbering, alerts and math: CommonMark list numbers,
// GitHub's top-level alerts and GitLab's titled ones, `$`…`$` and `math`
// fences as the forges delimit them.
export { markedCommonmarkList } from './parser/extensions/list';
export { markedGithubAlert, markedGitlabAlert } from './parser/extensions/alert';
export { markedGithubMath, markedGitlabMath } from './parser/extensions/math';
export { markedTableTaskCells } from './parser/extensions/tableTaskCells';
export { markedDescriptionLists } from './parser/extensions/descriptionLists';
export { markedJsonTable } from './parser/extensions/jsonTable';
// GitLab's playable images (`![clip](demo.mp4)` is a player), for a surface
// showing GitLab content.
export { markedMediaImages } from './parser/extensions/mediaImages';
export type { ImageMedia, MediaImageToken } from './parser/extensions/mediaImages';
// GitHub's theme-specific images: the renderer's and the sanitizer's
// `colorScheme`, and the `#gh-*-mode-only` reading a host claim keeps.
export { colorSchemeOnly, firstSrcsetCandidate } from './parser/extensions/themedImages';
export type { ColorScheme, ColorSchemeSources } from './parser/extensions/themedImages';
export {
  MEDIA_CLAIM_ATTR,
  MEDIA_KIND_ATTR,
  parseHtmlRegion,
  sanitizeEmbeddedHtmlToken,
} from './render/htmlSanitize';
export type { SanitizeEmbeddedHtmlOptions } from './render/htmlSanitize';
// The destination seam: the hooks a surface's extensions claim links and
// images with, which the engine and the HTML sanitizer both run.
export { destinationsOf } from './parser/lexer';
export type { DestinationContext, DestinationHook } from './parser/engine/destination';
// The URL a destination is read as (`decideDestination`), and whether an
// image's URL plays: a host scan of raw text reads a destination the same way.
export { backpedalUrl, canonicalHref } from './parser/engine/destination';
export { mediaOfHref } from './parser/extensions/mediaImages';
// Each forge's bare URL autolinks.
export { markedGithubBareUrls, markedGitlabBareUrls } from './parser/extensions/bareUrls';
export { imageSizeOf } from './render/imageSize';
export type { ImageSize } from './render/imageSize';
export { acquireDocumentInteraction } from './render/documentInteraction';
export type { DocumentInteraction } from './render/documentInteraction';
export { attachStreamdownLiteralHost, streamdownLiteralHostOf } from './render/literalHost';
export type {
  StreamdownLiteralHost,
  StreamdownLiteralHostHandle,
} from './render/literalHost';
export { DENIED_LINK_SCHEMES, isOpenableScheme, urlScheme } from './render/elements/urlSchemes';
// The renderers' own decisions about what text a token shows, for find's
// projection of rendered markdown (chat/markdown/markdownFindText.ts).
export { linkTagText, tableCellRenders } from './render/elements/url';
// The renderers' unavailable-image chip, which a host showing a claimed
// image that cannot load draws the same way.
export { UNAVAILABLE_IMAGE_CLASS, unavailableImageText } from './render/elements/url';
// What a whole document defines, for each block's lex: reference
// definitions everywhere, and a forge's footnotes, heading anchors and
// table of contents (parser/documentContext.ts).
export { createDocumentContextCache, documentContextOf, emptyDocumentContext, mayNeedDocumentContext } from './parser/index';
export type { DocumentContext, DocumentContextCache, DocumentForge } from './parser/index';
// What a host knows about the names a document mentions, read by a
// host's tokenizer through its block's scope (parser/documentScope.ts).
export { factDenied, readFact } from './parser/index';
export type { DocumentReading, ForgeFactReader, ForgeFactState } from './parser/index';
// A label's chip, which a host's extension builds and both renderers draw.
export type { LabelChipToken } from './parser/extensions/labelChip';
// A file excerpt written inline that stands as a block (a forge permalink):
// the host's tokenizer lifts its inline token to the snippet.
export { liftToBlock } from './parser/extensions/codeSnippet';
export type { CodeSnippetToken } from './parser/extensions/codeSnippet';
