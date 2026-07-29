// Composer @mention popover state + trigger detection.
//
// Owns:
//   - mention trigger / results list / active index / loading flag
//   - search-generation counter so a slow SearchWorkspaceFiles response
//     can't overwrite fresher results
//
// The caller provides a textarea reference + thread-id getter. UI
// rendering lives in Composer.svelte / ComposerMentionPopover — this
// module is purely state + dispatch.

import { SearchWorkspaceFiles } from '../../stores/bindings';
import { addToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import type { WorkspaceFile, WorkspaceFileSearchResult } from '../../types/workspaceFile';
import { detectMentionTrigger, type MentionTrigger } from './mentionHelpers';
import { replaceTextareaRange } from './textareaEdit';

export interface ComposerMentionsOptions {
  /** Returns the textarea DOM element. May be undefined before mount. */
  getTextarea: () => HTMLTextAreaElement | undefined;
  /** Thread-id getter — used to scope search requests. */
  getThreadId: () => string | null;
}

export interface ComposerMentionsHandle {
  readonly mentionTrigger: MentionTrigger | null;
  readonly mentionResults: WorkspaceFile[];
  readonly mentionActiveIndex: number;
  readonly mentionLoading: boolean;
  setMentionActiveIndex(i: number): void;

  /**
   * Inspect the textarea's value + caret and open / move / close the
   * mention popover.
   */
  refreshTriggers(): void;

  insertMention(file: WorkspaceFile): void;
  closeMention(): void;
}

export function createComposerMentions(opts: ComposerMentionsOptions): ComposerMentionsHandle {
  let mentionTrigger: MentionTrigger | null = $state(null);
  let mentionResults: WorkspaceFile[] = $state([]);
  let mentionActiveIndex = $state(0);
  let mentionLoading = $state(false);
  let mentionSearchGeneration = 0;

  async function loadMentionResults(query: string): Promise<void> {
    const threadId = opts.getThreadId();
    if (!threadId) {
      mentionResults = [];
      return;
    }
    const generation = ++mentionSearchGeneration;
    mentionLoading = true;
    try {
      const result = (await SearchWorkspaceFiles(threadId, query, 50)) as WorkspaceFileSearchResult;
      if (generation !== mentionSearchGeneration) return;
      mentionResults = result?.files ?? [];
      mentionActiveIndex = 0;
    } catch (err) {
      if (generation !== mentionSearchGeneration) return;
      console.error('SearchWorkspaceFiles failed:', err);
      mentionResults = [];
      addToast('warning', `Workspace search failed: ${errString(err)}`);
    } finally {
      if (generation === mentionSearchGeneration) {
        mentionLoading = false;
      }
    }
  }

  function closeMention(): void {
    mentionTrigger = null;
    mentionResults = [];
    mentionActiveIndex = 0;
    mentionSearchGeneration++;
  }

  function refreshTriggers(): void {
    const textarea = opts.getTextarea();
    if (!textarea) return;
    const value = textarea.value;
    const caret = textarea.selectionStart ?? value.length;

    const mention = detectMentionTrigger(value, caret);
    if (mention) {
      mentionTrigger = mention;
      void loadMentionResults(mention.query);
      return;
    }

    closeMention();
  }

  function insertMention(file: WorkspaceFile): void {
    const textarea = opts.getTextarea();
    if (!mentionTrigger || !textarea) return;
    // replaceTextareaRange routes the replacement through the browser's
    // input pipeline, which keeps it in the native undo stack. The resulting
    // `input` event drives `handleInput` in Composer.svelte, which calls
    // `draft.setContent(textarea.value)` — store update is automatic.
    replaceTextareaRange(textarea, mentionTrigger.start, mentionTrigger.end, `@${file.path} `);
    closeMention();
  }

  return {
    get mentionTrigger() { return mentionTrigger; },
    get mentionResults() { return mentionResults; },
    get mentionActiveIndex() { return mentionActiveIndex; },
    get mentionLoading() { return mentionLoading; },
    setMentionActiveIndex(i: number): void { mentionActiveIndex = i; },

    refreshTriggers,
    insertMention,
    closeMention,
  };
}
