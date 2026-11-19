// How ChatMarkdown parses and sanitizes a body, built in one place so the
// renderer and find's projection of it (markdownFindText.ts) read the same
// text: the parser extensions in their order, and the options the embedded
// HTML sanitizer runs with. A surface that renders one PR's content
// (FORGE_ATTACHMENT_SOURCE_CONTEXT) claims forge-hosted media as tokens
// (`buildForgeAttachmentExtension`) and, inside an HTML block, through the
// sanitizer's `claimMedia`, which drops a claimed video's children.

import type { ClaimEmbeddedMedia, ColorScheme, Extension, SanitizeEmbeddedHtmlOptions } from '../../../markdown';
import { EMBEDDED_HTML_EXTENSIONS, markedImageAttributes, markedMediaImages, markedSoftBreak } from '../../../markdown';
import {
  markedColorChips,
  markedCommonmarkList,
  markedCssColorChips,
  markedDescriptionLists,
  markedForgeGrammar,
  markedFrontMatter,
  markedGithubAlert,
  markedGithubMath,
  markedGitlabAlert,
  markedGitlabMath,
  markedInapplicableTaskList,
  markedInlineDiff,
  markedJsonTable,
  markedMultilineBlockquote,
  markedStrikethrough,
  markedTableTaskCells,
} from '../../../markdown';
import { getResolvedTheme } from '../../../stores/themeMode.svelte';
import { buildForgeAttachmentExtension } from '../../../utils/forgeAttachmentExtension';
import { buildForgeReferences } from '../../../utils/forgeReferenceExtension';
import { forgeHtmlOptions } from '../../../utils/forgeHtmlText';
import { buildForgeAttachmentHref, isForgeAttachmentHref, type ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { forgeEmojiGeneration, forgeEmojiSlot, resolveForgeEmoji } from '../../../utils/forgeEmoji.svelte';

// Each forge's own Markdown syntax: GFM's `~text~` strikethrough and its
// color chips on both, and GitLab's inline diff, `>>>` quotes, front
// matter, `[~]` tasks, task boxes in table cells, description lists and
// JSON tables. Then each forge's list numbering (CommonMark's; GitLab's
// task lists number the same way), alerts and math. GitLab's alerts follow
// its `>>>` quotes, which claim a `>>>` line first. Neither forge reads
// agent chat's own grammar (`markedForgeGrammar`).
const FORGE_SYNTAX: Record<'github' | 'gitlab', readonly Extension[]> = {
  github: [markedForgeGrammar, markedStrikethrough, markedColorChips, markedCommonmarkList, markedGithubAlert, ...markedGithubMath],
  gitlab: [
    markedForgeGrammar,
    markedStrikethrough,
    markedCssColorChips,
    markedInlineDiff,
    markedMultilineBlockquote,
    markedFrontMatter,
    markedInapplicableTaskList,
    markedTableTaskCells,
    markedDescriptionLists,
    markedJsonTable,
    markedGitlabAlert,
    ...markedGitlabMath,
  ],
};

export interface ChatMarkdownForgeOptions {
  /** The forge extensions, first in the list; none off a PR. The attachment
   * claims, then GitHub's soft-break `<br>` for embedded-HTML content or
   * GitLab's image attribute lists, then the forge's syntax (FORGE_SYNTAX),
   * then the forge's emoji shortcodes. */
  readonly extensions: readonly Extension[] | undefined;
  /** What the embedded HTML sanitizer runs with. */
  readonly html: SanitizeEmbeddedHtmlOptions | undefined;
  /** The reader's color scheme, which GitHub's theme-specific images
   * answer: the app's resolved theme, a reactive read. GitLab shows every
   * image's own src, so it has none. The renderer takes its value. */
  readonly colorScheme: (() => ColorScheme) | undefined;
}

/** What one PR's forge adds to the parse and the sanitizer; nothing
 * without a GitHub or GitLab source. ChatMarkdown keys this on the
 * source's scalars, so the extension identities hold across renders. */
export function chatMarkdownForgeOptions(source: ForgeAttachmentSource | null | undefined, embeddedHtml: boolean): ChatMarkdownForgeOptions {
  if (!source || (source.pr.forge !== 'github' && source.pr.forge !== 'gitlab')) return { extensions: undefined, html: undefined, colorScheme: undefined };
  const { pr, backend, webBase } = source;
  const claimMedia: ClaimEmbeddedMedia = (_tag, src) =>
    isForgeAttachmentHref(pr.forge, src) ? buildForgeAttachmentHref({ href: src, pr, backend, webBase }) : null;
  // References after attachments, which claim a relative `/uploads/` link
  // the reference extension would otherwise resolve as a repository path.
  const references = buildForgeReferences(source);
  const attachments: readonly Extension[] = [
    ...buildForgeAttachmentExtension({ pr, backend, webBase, embeddedHtml }),
    references.extension,
  ];
  // GitHub renders a soft line break in a PR body or comment as <br>;
  // GitLab renders a space, as CommonMark does, and sizes an image by the
  // attribute list after it, and plays an image of a video or audio file.
  // These extensions are module constants, so the list's identities hold
  // for one PR.
  const forgeOwn = pr.forge === 'github' ? (embeddedHtml ? [markedSoftBreak] : []) : [markedMediaImages, markedImageAttributes];
  const colorScheme = pr.forge === 'github' ? getResolvedTheme : undefined;
  const emoji = forgeEmojiSlot(pr.forge);
  // Text and links inside HTML get what the forge gives them in a
  // paragraph: references, then emoji, read when a run is (forgeHtmlText.ts).
  const htmlText = forgeHtmlOptions(source, references, () => [references.extension, resolveForgeEmoji(emoji)]);
  // Emoji shortcodes last: a module-constant slot that
  // `chatMarkdownExtensions` resolves once the name table has loaded.
  return {
    extensions: [...attachments, ...forgeOwn, ...FORGE_SYNTAX[pr.forge], emoji],
    // GitHub shows a `<textarea>` or `<iframe>` as its source (htmlSanitize.ts `tagFilter`).
    html: pr.forge === 'github' ? { claimMedia, colorScheme, tagFilter: true, ...htmlText } : { claimMedia, ...htmlText },
    colorScheme,
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
  // A forge emoji slot becomes the loaded extension when its table arrives;
  // reading it here makes that a reactive change of this list alone.
  if (parts.forge) for (const extension of parts.forge) list.push(resolveForgeEmoji(extension));
  if (parts.preview) list.push(parts.preview);
  if (parts.pathLink) list.push(parts.pathLink);
  if (parts.embeddedHtml) list.push(...EMBEDDED_HTML_EXTENSIONS);
  return list;
}

/** Changes when a lazily loaded part of these options arrives (the forge
 * emoji table) or the theme a GitHub surface's `colorScheme` reads moves,
 * so a holder of text projected under the old options drops it. */
export function chatMarkdownOptionsGeneration(): string {
  return `${forgeEmojiGeneration()}:${getResolvedTheme()}`;
}
