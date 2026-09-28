// Composer @mention popover state + trigger detection.
//
// Owns:
//   - mention trigger / results list / active index / loading flag / error
//   - search-generation counter so a slow SearchWorkspaceFiles response
//     can't overwrite fresher results
//   - the dismissed trigger, so Escape holds until the text stops matching
//
// The caller provides a textarea reference + workspace getter. The search
// reads a CHECKOUT, so it is keyed on the workspace and works on a draft
// placeholder exactly as it does on a persisted thread. UI rendering lives
// in Composer.svelte / ComposerMentionPopover — this module is purely state
// + dispatch.

import { SearchWorkspaceFiles } from '../../stores/bindings';
import type { WorkspaceRef } from '../../types/git';
import { errString } from '../../utils/errors';
import type { WorkspaceFile, WorkspaceFileSearchResult } from '../../types/workspaceFile';
import { detectMentionTrigger, type MentionTrigger } from './mentionHelpers';
import { replaceTextareaRange } from './textareaEdit';

export interface ComposerMentionsOptions {
  /** Returns the textarea DOM element. May be undefined before mount. */
  getTextarea: () => HTMLTextAreaElement | undefined;
  /** Workspace getter — the checkout the search runs in. */
  getWorkspace: () => WorkspaceRef | null;
}

export interface ComposerMentionsHandle {
  readonly mentionTrigger: MentionTrigger | null;
  readonly mentionResults: WorkspaceFile[];
  readonly mentionActiveIndex: number;
  readonly mentionLoading: boolean;
  /** Non-empty when the last search failed; rendered inside the popover. */
  readonly mentionError: string;
  setMentionActiveIndex(i: number): void;

  /**
   * Inspect the textarea's value + caret and open / move / close the
   * mention popover.
   */
  refreshTriggers(): void;

  insertMention(file: WorkspaceFile): void;
  /** Dismiss the popover until the text stops matching this trigger. */
  closeMention(): void;
}

/** What a search answers for: the checkout, the `@` position, the query. */
function mentionSearchKey(workspace: WorkspaceRef | null, trigger: MentionTrigger): string {
  return `${workspace?.projectId ?? ''}\0${workspace?.workspacePath ?? ''}\0${trigger.start}\0${trigger.query}`;
}

export function createComposerMentions(opts: ComposerMentionsOptions): ComposerMentionsHandle {
  let mentionTrigger: MentionTrigger | null = $state(null);
  let mentionResults: WorkspaceFile[] = $state([]);
  let mentionActiveIndex = $state(0);
  let mentionLoading = $state(false);
  let mentionError = $state('');
  let mentionSearchGeneration = 0;
  // Key of the search the open popover shows. Selection, keyup and click
  // refresh the trigger without changing it; those must not search again.
  let openSearchKey: string | null = null;
  // `@` index of the trigger the user dismissed, or null.
  let dismissedStart: number | null = null;

  async function loadMentionResults(workspace: WorkspaceRef | null, query: string): Promise<void> {
    const generation = ++mentionSearchGeneration;
    if (!workspace) {
      mentionResults = [];
      mentionLoading = false;
      return;
    }
    mentionLoading = true;
    try {
      const result = (await SearchWorkspaceFiles(workspace, query, 50)) as WorkspaceFileSearchResult;
      if (generation !== mentionSearchGeneration) return;
      mentionResults = result?.files ?? [];
      mentionActiveIndex = 0;
      mentionError = '';
    } catch (err) {
      if (generation !== mentionSearchGeneration) return;
      mentionResults = [];
      mentionError = errString(err);
    } finally {
      if (generation === mentionSearchGeneration) {
        mentionLoading = false;
      }
    }
  }

  function resetMention(): void {
    mentionTrigger = null;
    mentionResults = [];
    mentionActiveIndex = 0;
    mentionLoading = false;
    mentionError = '';
    openSearchKey = null;
    mentionSearchGeneration++;
  }

  function closeMention(): void {
    // Escape reaches both the textarea and the popover's own handler; the
    // second close finds nothing open and must keep the first dismissal.
    if (!mentionTrigger) return;
    dismissedStart = mentionTrigger.start;
    resetMention();
  }

  function refreshTriggers(): void {
    const textarea = opts.getTextarea();
    if (!textarea) return;
    const value = textarea.value;
    const caret = textarea.selectionStart ?? value.length;

    const mention = detectMentionTrigger(value, caret);
    if (!mention || mention.start !== dismissedStart) dismissedStart = null;
    if (!mention || dismissedStart !== null) {
      if (mentionTrigger) resetMention();
      return;
    }
    const workspace = opts.getWorkspace();
    const key = mentionSearchKey(workspace, mention);
    if (key === openSearchKey) return;
    openSearchKey = key;
    mentionTrigger = mention;
    void loadMentionResults(workspace, mention.query);
  }

  function insertMention(file: WorkspaceFile): void {
    const textarea = opts.getTextarea();
    if (!mentionTrigger || !textarea) return;
    // replaceTextareaRange routes the replacement through the browser's
    // input pipeline, which keeps it in the native undo stack. The resulting
    // `input` event drives `handleInput` in Composer.svelte, which calls
    // `draft.setContent(textarea.value)` — store update is automatic.
    replaceTextareaRange(textarea, mentionTrigger.start, mentionTrigger.end, `@${file.path} `);
    resetMention();
  }

  return {
    get mentionTrigger() { return mentionTrigger; },
    get mentionResults() { return mentionResults; },
    get mentionActiveIndex() { return mentionActiveIndex; },
    get mentionLoading() { return mentionLoading; },
    get mentionError() { return mentionError; },
    setMentionActiveIndex(i: number): void { mentionActiveIndex = i; },

    refreshTriggers,
    insertMention,
    closeMention,
  };
}
