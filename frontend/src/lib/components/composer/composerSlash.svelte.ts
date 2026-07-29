// Composer slash-command menu state (D31).
//
// Owns: the open trigger (which implies the filtered result list) and the
// active index. There is no async work — the registry is a static list in
// `slashCommands.ts` — so unlike the @-mention twin this module has no
// generation counter and no loading flag.
//
// The caller provides a textarea reference. Rendering lives in
// Composer.svelte / ComposerSlashPopover; this module is state + dispatch.

import { detectSlashTrigger, slashCommandWord, type SlashCommand, type SlashTrigger } from './slashCommands';
import { replaceTextareaRange } from './textareaEdit';

export interface ComposerSlashOptions {
  /** Returns the textarea DOM element. May be undefined before mount. */
  getTextarea: () => HTMLTextAreaElement | undefined;
}

export interface ComposerSlashHandle {
  readonly slashTrigger: SlashTrigger | null;
  readonly slashResults: SlashCommand[];
  readonly slashActiveIndex: number;
  setSlashActiveIndex(i: number): void;

  /**
   * Inspect the textarea's value + caret and open / filter / close the
   * command menu.
   */
  refreshTrigger(): void;

  insertCommand(command: SlashCommand): void;
  closeSlash(): void;
}

export function createComposerSlash(opts: ComposerSlashOptions): ComposerSlashHandle {
  let slashTrigger: SlashTrigger | null = $state(null);
  let slashActiveIndex = $state(0);
  // Set while the menu is deliberately dismissed (Escape) for a draft that
  // still matches. Cleared as soon as the draft stops matching, so the next
  // `/` types a fresh menu rather than staying suppressed forever.
  let dismissed = $state(false);

  function closeSlash(): void {
    slashTrigger = null;
    slashActiveIndex = 0;
    dismissed = true;
  }

  function refreshTrigger(): void {
    const textarea = opts.getTextarea();
    if (!textarea) return;
    const value = textarea.value;
    const caret = textarea.selectionStart ?? value.length;
    const next = detectSlashTrigger(value, caret);
    if (!next) {
      slashTrigger = null;
      slashActiveIndex = 0;
      dismissed = false;
      return;
    }
    if (dismissed) return;
    // Keep the highlighted row inside the (possibly narrowed) result list.
    if (slashActiveIndex >= next.results.length) slashActiveIndex = 0;
    slashTrigger = next;
  }

  function insertCommand(command: SlashCommand): void {
    const textarea = opts.getTextarea();
    if (!slashTrigger || !textarea) return;
    // Trailing space: the command word is complete, so the caret belongs at
    // the start of the instruction the user is about to type — and the space
    // is what closes the trigger on the next refresh.
    replaceTextareaRange(textarea, slashTrigger.start, slashTrigger.end, `${slashCommandWord(command)} `);
    slashTrigger = null;
    slashActiveIndex = 0;
    dismissed = false;
  }

  return {
    get slashTrigger() { return slashTrigger; },
    get slashResults() { return slashTrigger?.results ?? []; },
    get slashActiveIndex() { return slashActiveIndex; },
    setSlashActiveIndex(i: number): void { slashActiveIndex = i; },

    refreshTrigger,
    insertCommand,
    closeSlash,
  };
}
