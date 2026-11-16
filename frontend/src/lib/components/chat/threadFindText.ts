// A timeline row's find text: the text thread find counts and paints in
// it. Each row kind's renderer marks the elements that show it with
// `data-find-text`, and the highlighter paints only text inside
// them (ThreadPaneFind); `rowFindRuns` projects the same text from the
// row, so a row the bar counts holds the matches it paints. A row
// projects from the same functions its renderer shows text by.
//
// Find text, by what the row shows by default:
//   - user_text: the bubble's text (userMessageVisibleText), without image
//     blocks and the `!command` a shell output's block shows;
//   - assistant_text: the rendered markdown (markdownFindText under the
//     chat options, after the raw-JSON fence AssistantMessage applies); a
//     generated image or a question card has none;
//   - error: its text;
//   - tool_call, tool_completion: a command row's header (its description
//     or its command), or a generic card's argument preview; a preview of a
//     path, shown relative to the workspace, or of an AO tool's argument,
//     shown by this client's names for it, is not one, and no other card
//     has one;
//   - thinking, compaction_reasoning: the body the reader expanded. The
//     collapsed tail is not: which lines it shows depends on the pane's
//     width.
// Every other kind, the labels, timestamps, badges and controls of every
// row, and what a row shows only when expanded (command output, diffs) is
// not find text.
//
// The store's candidates (internal/store/find_candidate.go) hold every row
// whose find text can match; findCandidateVectors.json holds the two sides
// to each other (threadFindText.browser.test.ts).

import type { Item } from '../../types/models';
import { parseJsonObject } from '../../utils/parseJsonObject';
import { generatedImageRow } from '../../utils/generatedImageMeta';
import { parseUserMessageAttachments, userMessageVisibleText } from '../../utils/userMessageMeta';
import {
  COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY,
  THINKING_PAYLOAD_EXPANSION_STATE_KEY,
} from '../../utils/payloadVersion';
import { aoToolServer } from './aoTools';
import { commandHeaderText } from './commandDisplay';
import { createMarkdownFindTextCache } from './markdown/markdownFindText';
import { RawJsonFenceFormatter } from './markdown/rawJsonFence';
import { reasoningBodyText } from './reasoningTailSource';
import { presentToolCardInputPreview } from './toolCardPreview';
import { resolveToolPresentation } from './toolPresentation';

export const FIND_TEXT_SELECTOR = '[data-find-text]';

export interface FindTextContext {
  /** The thread's provider, which picks a tool row's presentation. */
  provider: string | undefined;
  /** The pane's workspace path, which a tool row's preview is shown against. */
  workspacePath: string;
  /** The payload text of a body the reader expanded (RowUiRegistry.expandedItemData). */
  expandedItemData(itemId: string, stateKey: string): string | null | undefined;
  /** The whole live text of a reasoning row still revealing, or null. */
  liveReasoningText(itemId: string): string | null;
  /** Whether a row's reveal still drains after it settled. */
  isItemSmoothing(itemId: string): boolean;
}

const REASONING_STATE_KEYS: Record<string, string> = {
  thinking: THINKING_PAYLOAD_EXPANSION_STATE_KEY,
  compaction_reasoning: COMPACTION_REASONING_PAYLOAD_EXPANSION_STATE_KEY,
};

const assistantRuns = createMarkdownFindTextCache({ embeddedHtml: false });

/** The text AssistantMessage gives ChatMarkdown for a settled row. */
export function assistantMarkdownSource(summary: string): string {
  return new RawJsonFenceFormatter().render(summary, false);
}

function assistantFindRuns(item: Item): readonly string[] {
  if (generatedImageRow(item) !== null) return [];
  if (Array.isArray(parseJsonObject(item.meta)?.questions)) return [];
  return assistantRuns(assistantMarkdownSource(item.summary)).runs;
}

function toolFindRuns(item: Item, context: FindTextContext): readonly string[] {
  const presentation = resolveToolPresentation({
    item,
    provider: context.provider,
    surface: 'timeline',
    workspacePath: context.workspacePath,
  });
  if (presentation.kind === 'command') {
    const text = commandHeaderText(presentation.item, presentation.meta);
    return text ? [text] : [];
  }
  if (presentation.kind !== 'generic') return [];
  const meta = parseJsonObject(item.meta);
  if (aoToolServer(meta)) return [];
  const preview = presentToolCardInputPreview(item, parseJsonObject(item.payloadMeta), meta, context.workspacePath);
  return preview.path || !preview.text ? [] : [preview.text];
}

function reasoningFindRuns(item: Item, context: FindTextContext): readonly string[] {
  const data = context.expandedItemData(item.id, REASONING_STATE_KEYS[item.kind]);
  if (data === undefined) return [];
  const body = reasoningBodyText({
    summary: item.summary ?? '',
    liveWindow: null,
    liveText: () => context.liveReasoningText(item.id),
    persisted: data ?? '',
    expanded: true,
    isStreaming: item.status === 'streaming' || context.isItemSmoothing(item.id),
  });
  return body.text ? [body.text] : [];
}

/** The runs of text a row's find text shows, in document order. */
export function rowFindRuns(item: Item, context: FindTextContext): readonly string[] {
  switch (item.kind) {
    case 'user_text': {
      const text = userMessageVisibleText(item.summary, parseUserMessageAttachments(item.meta, item.threadId));
      return text ? [text] : [];
    }
    case 'assistant_text':
      return assistantFindRuns(item);
    case 'error':
      return item.summary ? [item.summary] : [];
    case 'tool_call':
    case 'tool_completion':
      return toolFindRuns(item, context);
    case 'thinking':
    case 'compaction_reasoning':
      return reasoningFindRuns(item, context);
    default:
      return [];
  }
}

/** Whether a row expands to show more find text than it holds now. */
export function rowFindFollowsExpansion(item: Item): boolean {
  return item.kind in REASONING_STATE_KEYS;
}
