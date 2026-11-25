// How ChatMarkdown parses and sanitizes a body, built in one place so the
// renderer and find's projection of it (markdownFindText.ts) read the same
// text: the parser extensions in their order, and the options the embedded
// HTML sanitizer runs with. A surface that renders one PR's content
// (FORGE_ATTACHMENT_SOURCE_CONTEXT) claims forge-hosted media and resolves
// relative links with destination hooks, which the parse and, inside an
// HTML block, the sanitizer both run (`destinationsOf`).

import type { ColorScheme, Extension, SanitizeEmbeddedHtmlOptions } from '../../../markdown';
import { destinationsOf, embeddedHtmlFor, markedImageAttributes, markedMediaImages, markedSoftBreak } from '../../../markdown';
import {
  markedColorChips,
  markedCommonmarkList,
  markedCssColorChips,
  markedDescriptionLists,
  markedForgeGrammar,
  markedFrontMatter,
  markedGithubAlert,
  markedGithubBareUrls,
  markedGithubMath,
  markedGitlabAlert,
  markedGitlabBareUrls,
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
import { buildForgeGitlabReferences, buildForgeReferences } from '../../../utils/forgeReferenceExtension';
import { buildForgeRepositoryFiles } from '../../../utils/forgeRepositoryFiles';
import { forgeHtmlOptions } from '../../../utils/forgeHtmlText';
import type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';
import { CHAT_IMAGE_PREFIXES, CHAT_LINK_PREFIXES } from './chatLinkPrefixes';
import { forgeEmojiGeneration, forgeEmojiSlot, resolveForgeEmoji } from '../../../utils/forgeEmoji.svelte';
import { trackerMediaExtension } from '../../../utils/trackerMedia';

// Each forge's own Markdown syntax: its bare URL autolinks, GFM's `~text~`
// strikethrough and its color chips on both, and GitLab's inline diff, `>>>` quotes, front
// matter, `[~]` tasks, task boxes in table cells, description lists and
// JSON tables. Then each forge's list numbering (CommonMark's; GitLab's
// task lists number the same way), alerts and math. GitLab's alerts follow
// its `>>>` quotes, which claim a `>>>` line first. Neither forge reads
// agent chat's own grammar (`markedForgeGrammar`).
const FORGE_SYNTAX: Record<'github' | 'gitlab', readonly Extension[]> = {
  github: [markedForgeGrammar, markedGithubBareUrls, markedStrikethrough, markedColorChips, markedCommonmarkList, markedGithubAlert, ...markedGithubMath],
  gitlab: [
    markedForgeGrammar,
    markedGitlabBareUrls,
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

/** The sanitizer admits an `href` or `src` by the renderer's own prefixes. */
const HTML_PREFIXES = { allowedLinkPrefixes: [...CHAT_LINK_PREFIXES], allowedImagePrefixes: [...CHAT_IMAGE_PREFIXES] };

export interface ChatMarkdownForgeOptions {
  /** The forge extensions, first in the list; none off a PR. The attachment
   * claims, then GitHub's soft-break `<br>` for embedded-HTML content or
   * GitLab's image attribute lists, then the forge's syntax (FORGE_SYNTAX),
   * then the forge's emoji shortcodes. */
  readonly extensions: readonly Extension[] | undefined;
  /** The embedded-HTML extensions, last in the list; none without
   * `embeddedHtml`. Their inline tokenizer keeps the elements the
   * sanitizer's `html.policy` keeps (`embeddedHtmlFor`). */
  readonly htmlExtensions: readonly Extension[] | undefined;
  /** What the embedded HTML sanitizer runs with: the policy, the
   * extensions' destination hooks and the renderer's URL prefixes. */
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
  if (!source || (source.pr.forge !== 'github' && source.pr.forge !== 'gitlab')) {
    const any = embeddedHtml ? embeddedHtmlFor(null) : undefined;
    return { extensions: undefined, htmlExtensions: any?.extensions, html: any && { policy: any.policy, ...HTML_PREFIXES }, colorScheme: undefined };
  }
  const { pr, backend, webBase } = source;
  const embedded = embeddedHtmlFor(pr.forge);
  // References after attachments, whose destination hook claims a relative
  // `/uploads/` link the references' hook would resolve as a repository path.
  const references = buildForgeReferences(source);
  // GitLab's labels, milestones, snippets and epics, ahead of the forge
  // strikethrough in FORGE_SYNTAX, which it offers each tilde first.
  const gitlabReferences = buildForgeGitlabReferences(source);
  // The repository's own images and permalink snippets, after attachments,
  // whose hook claims a relative `/uploads/` image first.
  const repositoryFiles = buildForgeRepositoryFiles(source);
  const attachments: readonly Extension[] = [
    ...buildForgeAttachmentExtension({ pr, backend, webBase, embeddedHtml }),
    ...(repositoryFiles ? [repositoryFiles] : []),
    references,
    ...(gitlabReferences ? [gitlabReferences] : []),
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
  // paragraph: references (GitLab's labels and the rest after them), then
  // emoji, read when a run is (forgeHtmlText.ts).
  const htmlText = forgeHtmlOptions(source, () =>
    gitlabReferences ? [references, gitlabReferences, resolveForgeEmoji(emoji)] : [references, resolveForgeEmoji(emoji)],
  );
  // Emoji shortcodes last: a module-constant slot that
  // `chatMarkdownExtensions` resolves once the name table has loaded.
  const extensions = [...attachments, ...forgeOwn, ...FORGE_SYNTAX[pr.forge], emoji];
  return {
    extensions,
    htmlExtensions: embeddedHtml ? embedded.extensions : undefined,
    html: { policy: embedded.policy, destinations: destinationsOf(extensions), colorScheme, ...HTML_PREFIXES, ...htmlText },
    colorScheme,
  };
}

// A ticket's text claims its tracker media (utils/trackerMedia.ts), in
// Markdown and, through the same hook, inside embedded HTML.
const TRACKER_EXTENSIONS: readonly Extension[] = [trackerMediaExtension];

/** What a ticket's text adds to the parse and the sanitizer, in the shape
 * a PR's forge options take (a ticket is no PR, so it reads no forge
 * source): the tracker media claim. Module constants, so the identities
 * hold across renders. */
export function chatMarkdownTrackerOptions(embeddedHtml: boolean): ChatMarkdownForgeOptions {
  const any = embeddedHtml ? embeddedHtmlFor(null) : undefined;
  return {
    extensions: TRACKER_EXTENSIONS,
    htmlExtensions: any?.extensions,
    html: any && { policy: any.policy, destinations: destinationsOf(TRACKER_EXTENSIONS), ...HTML_PREFIXES },
    colorScheme: undefined,
  };
}

/** ChatMarkdown's parser extensions in order, which is the order their
 * destination hooks claim in: forge attachments, then the repository's
 * images, then the forge's relative links, then preview, then path links. The forge's `<video>` tokenizer
 * precedes the embedded-HTML pair, which would otherwise take its start. */
export function chatMarkdownExtensions(parts: {
  forge: ChatMarkdownForgeOptions;
  preview?: Extension;
  pathLink?: Extension;
}): Extension[] {
  const list: Extension[] = [];
  // A forge emoji slot becomes the loaded extension when its table arrives;
  // reading it here makes that a reactive change of this list alone.
  if (parts.forge.extensions) for (const extension of parts.forge.extensions) list.push(resolveForgeEmoji(extension));
  if (parts.preview) list.push(parts.preview);
  if (parts.pathLink) list.push(parts.pathLink);
  if (parts.forge.htmlExtensions) list.push(...parts.forge.htmlExtensions);
  return list;
}

/** Changes when a lazily loaded part of these options arrives (the forge
 * emoji table) or the theme a GitHub surface's `colorScheme` reads moves,
 * so a holder of text projected under the old options drops it. */
export function chatMarkdownOptionsGeneration(): string {
  return `${forgeEmojiGeneration()}:${getResolvedTheme()}`;
}
