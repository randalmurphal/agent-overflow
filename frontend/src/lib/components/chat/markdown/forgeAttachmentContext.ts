// Svelte context key for "this markdown belongs to a pull/merge request".
//
// `ReviewPane` sets it once for its whole subtree; every `ChatMarkdown`
// inside — the PR body, the conversation, a thread's comments, the comments
// list — reads it and builds the forge-attachment extension from it. That is
// why none of the five call sites take a prop: the fact is the PANE's, not
// each row's, and a prop chain would have to thread through ReviewRail and
// ReviewPRThreadRow to reach the same answer.
//
// A function rather than a value: the pane's PR reference, its computer and
// its web URL all arrive asynchronously, and context is set once at mount.
// Surfaces that never set it (agent chat, settings previews) read
// `undefined` and build no extension.
//
// Exported as a string constant, like `CHAT_MARKDOWN_SETTLED_CONTEXT`, so a
// test can register a source without importing the review tree.
export const FORGE_ATTACHMENT_SOURCE_CONTEXT = 'agent-overflow:forge-attachment-source';

export type { ForgeAttachmentSource } from '../../../utils/forgeAttachments';

/** What `getContext(FORGE_ATTACHMENT_SOURCE_CONTEXT)` answers. */
export type ForgeAttachmentSourceReader = () =>
  | import('../../../utils/forgeAttachments').ForgeAttachmentSource
  | null;
