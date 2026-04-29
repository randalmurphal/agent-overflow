// Composer @mention + /slash popover state + trigger detection.
//
// Extracted from Composer.svelte. Owns:
//   - mention trigger / results list / active index / loading flag
//   - slash trigger / commands cache / active index
//   - search-generation counter so a slow SearchWorkspaceFiles response
//     can't overwrite fresher results
//   - per-thread slash-commands cache that resets on thread switch
//
// The caller provides a textarea reference + thread-id getter. All UI
// rendering still lives in Composer.svelte / ComposerMentionPopover /
// ComposerSlashPopover — this module is purely state + dispatch.

import { GetThreadSlashCommands, SearchWorkspaceFiles } from '../../stores/bindings';
import { addToast } from '../../stores/toast.svelte';
import { errString } from '../../utils/errors';
import type { WorkspaceFile, WorkspaceFileSearchResult } from '../../types/workspaceFile';
import { detectMentionTrigger, type MentionTrigger } from './mentionHelpers';
import { detectSlashTrigger, type SlashTrigger } from './slashHelpers';

export interface ComposerMentionsOptions {
  /** Returns the textarea DOM element. May be undefined before mount. */
  getTextarea: () => HTMLTextAreaElement | undefined;
  /** Thread-id getter — used to scope cache + search requests. */
  getThreadId: () => string | null;
}

export interface ComposerMentionsHandle {
  // Mention popover state
  readonly mentionTrigger: MentionTrigger | null;
  readonly mentionResults: WorkspaceFile[];
  readonly mentionActiveIndex: number;
  readonly mentionLoading: boolean;
  setMentionActiveIndex(i: number): void;

  // Slash popover state
  readonly slashTrigger: SlashTrigger | null;
  readonly slashFilteredCommands: string[];
  readonly slashActiveIndex: number;
  setSlashActiveIndex(i: number): void;

  /**
   * Inspect the textarea's value + caret and open / move / close the
   * appropriate popover. Both popovers are mutually exclusive — only the
   * rule that matches first fires. Debounced by the caller via input+
   * selection handlers, not internally.
   */
  refreshTriggers(): void;

  insertMention(file: WorkspaceFile): void;
  insertSlashCommand(command: string): void;
  closeMention(): void;
  closeSlash(): void;

  /**
   * Reset the slash-commands cache when the active thread changes. Called
   * from a $effect in Composer.svelte so the next slash trigger refetches
   * against the new thread.
   */
  onThreadChanged(threadId: string | null): void;
}

export function createComposerMentions(opts: ComposerMentionsOptions): ComposerMentionsHandle {
  let mentionTrigger: MentionTrigger | null = $state(null);
  let mentionResults: WorkspaceFile[] = $state([]);
  let mentionActiveIndex = $state(0);
  let mentionLoading = $state(false);
  let mentionSearchGeneration = 0;

  let slashTrigger: SlashTrigger | null = $state(null);
  let slashCommandsCache: string[] = $state([]);
  let slashActiveIndex = $state(0);
  let slashFetchedForThread: string | null = null;

  // Filter the slash command cache by the trigger text. Pure derivation —
  // the caller doesn't need an explicit refresh when the trigger updates.
  let slashFilteredCommands = $derived.by(() => {
    if (!slashTrigger) return [] as string[];
    const q = slashTrigger.text.toLowerCase();
    if (!q) return slashCommandsCache.slice();
    return slashCommandsCache.filter((cmd) => cmd.toLowerCase().includes(q));
  });

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

  async function ensureSlashCommandsLoaded(): Promise<void> {
    const threadId = opts.getThreadId();
    if (!threadId) {
      slashCommandsCache = [];
      slashFetchedForThread = null;
      return;
    }
    if (slashFetchedForThread === threadId) return;
    slashFetchedForThread = threadId;
    try {
      const result = (await GetThreadSlashCommands(threadId)) as string[];
      // Guard a thread-switch-in-flight: apply the result only if we're
      // still on the same thread when the RPC returns.
      if (opts.getThreadId() !== threadId) return;
      slashCommandsCache = Array.isArray(result) ? result : [];
      if (slashActiveIndex >= slashCommandsCache.length) {
        slashActiveIndex = 0;
      }
    } catch (err) {
      console.error('GetThreadSlashCommands failed:', err);
      if (opts.getThreadId() === threadId) {
        // Leave the cache empty; the popover falls back to its empty
        // state. `slashFetchedForThread` is set so we don't thrash the
        // binding on every keystroke.
        slashCommandsCache = [];
      }
    }
  }

  function closeMention(): void {
    mentionTrigger = null;
    mentionResults = [];
    mentionActiveIndex = 0;
    mentionSearchGeneration++;
  }

  function closeSlash(): void {
    slashTrigger = null;
    slashActiveIndex = 0;
  }

  function refreshTriggers(): void {
    const textarea = opts.getTextarea();
    if (!textarea) return;
    const value = textarea.value;
    const caret = textarea.selectionStart ?? value.length;

    const mention = detectMentionTrigger(value, caret);
    if (mention) {
      closeSlash();
      mentionTrigger = mention;
      void loadMentionResults(mention.query);
      return;
    }

    const slash = detectSlashTrigger(value, caret);
    if (slash) {
      closeMention();
      slashTrigger = slash;
      if (slashActiveIndex >= slashFilteredCommands.length) {
        slashActiveIndex = 0;
      }
      void ensureSlashCommandsLoaded();
      return;
    }

    closeMention();
    closeSlash();
  }

  function insertMention(file: WorkspaceFile): void {
    const textarea = opts.getTextarea();
    if (!mentionTrigger || !textarea) return;
    // execCommand routes the replacement through the browser's input
    // pipeline, which keeps it in the native undo stack. The synthetic
    // `input` event drives `handleInput` in Composer.svelte, which calls
    // `draft.setContent(textarea.value)` — store update is automatic.
    const replacement = `@${file.path} `;
    textarea.focus();
    textarea.setSelectionRange(mentionTrigger.start, mentionTrigger.end);
    document.execCommand('insertText', false, replacement);
    closeMention();
  }

  function insertSlashCommand(command: string): void {
    const textarea = opts.getTextarea();
    if (!slashTrigger || !textarea) return;
    const triggerEnd = slashTrigger.start + 1 + slashTrigger.text.length;
    const replacement = `/${command} `;
    textarea.focus();
    textarea.setSelectionRange(slashTrigger.start, triggerEnd);
    document.execCommand('insertText', false, replacement);
    closeSlash();
  }

  function onThreadChanged(threadId: string | null): void {
    if (threadId !== slashFetchedForThread) {
      slashCommandsCache = [];
      slashFetchedForThread = null;
      closeSlash();
    }
  }

  return {
    get mentionTrigger() { return mentionTrigger; },
    get mentionResults() { return mentionResults; },
    get mentionActiveIndex() { return mentionActiveIndex; },
    get mentionLoading() { return mentionLoading; },
    setMentionActiveIndex(i: number): void { mentionActiveIndex = i; },

    get slashTrigger() { return slashTrigger; },
    get slashFilteredCommands() { return slashFilteredCommands; },
    get slashActiveIndex() { return slashActiveIndex; },
    setSlashActiveIndex(i: number): void { slashActiveIndex = i; },

    refreshTriggers,
    insertMention,
    insertSlashCommand,
    closeMention,
    closeSlash,
    onThreadChanged,
  };
}
