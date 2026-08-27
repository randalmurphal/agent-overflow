<script lang="ts">
  // The composer's editing core: text entry + attachments. Everything the
  // host decides — send, thread lifecycle, pending prompts, toolbar — is
  // outside. See composerInputSurface.ts for the contract, and its doc
  // comment for what deliberately does NOT live here.

  import { flushSync, onDestroy } from 'svelte';
  import { paneWorkspacePath } from '../../stores/thread.svelte';
  import ComposerAttachmentRow from './ComposerAttachmentRow.svelte';
  import ComposerCommandHighlight from './ComposerCommandHighlight.svelte';
  import ComposerMentionPopover from './ComposerMentionPopover.svelte';
  import ComposerSlashPopover from './ComposerSlashPopover.svelte';
  import ComposerTerminalChipRow from './ComposerTerminalChipRow.svelte';
  import { dispatchComposerInputKeydown, focusTextareaAt, focusTextareaAtEnd } from './composerKeyboard';
  import { createComposerImagePlaceholders } from './composerImagePlaceholders';
  import { createComposerMentions } from './composerMentions.svelte';
  import { createComposerSlash } from './composerSlash.svelte';
  import { createComposerUploads } from './composerUploads.svelte';
  import { slashCommandMatches } from './slashCommands';
  import type {
    ComposerInputSelection,
    ComposerInputSurfaceProps,
  } from './composerInputSurface';

  let {
    pane,
    draft,
    value,
    disabled,
    placeholder,
    oninput,
    onSubmitEnter,
    onKeydown,
    onKeydownAfterPopovers,
    editsDraft = true,
    showDraftRows = true,
    blockAttachment,
    shouldDeleteAttachmentRecord,
    attachmentCache,
    uploadThreadId,
    ensureUploadThreadId,
    onImageExpand,
  }: ComposerInputSurfaceProps = $props();

  let textarea: HTMLTextAreaElement | undefined = $state(undefined);
  let lastAutosizedTextarea: HTMLTextAreaElement | undefined;
  let lastAutosizedValue = '';
  // Bumped by recreateInput() to swap the <textarea> element itself (the
  // {#key} below). Blink retains one edit command per character typed into a
  // text control for the ELEMENT's lifetime — no API clears it, and each
  // long-lived command pins a 128KB Oilpan page (measured 2026-08-24: ~50MB
  // after a day of composer use). A send is the natural boundary: the
  // programmatic clear has already emptied the native undo stack (measured —
  // Ctrl+Z restores nothing either way), so replacing the element costs no
  // behavior, and the same-flush refocus below keeps the swap invisible.
  let inputEpoch = $state(0);

  // The textarea grows with its content through `field-sizing: content`
  // (the `field-sizing-content` + `max-h-50` utilities on the element) and
  // the JS autosize below exists only for an engine without it. Measured
  // 2026-08-23 on the WebView2 build: the JS path cost every keystroke two
  // forced layouts (height:auto → scrollHeight, then the frame's own), and
  // an inline `height` would override the CSS sizing, so the two paths are
  // exclusive. Detected off the style object rather than `CSS.supports`,
  // which happy-dom answers true for every query; the unit suite runs the
  // fallback, the browser suite runs the native path.
  const nativeAutosize =
    typeof document !== 'undefined' && 'fieldSizing' in document.createElement('textarea').style;

  const mentions = createComposerMentions({
    getTextarea: () => textarea,
    getThreadId: () => pane.threadId,
  });

  const slash = createComposerSlash({ getTextarea: () => textarea, getPane: () => pane });

  const uploads = createComposerUploads({
    getThreadId: () => (uploadThreadId ? uploadThreadId() : pane.threadId),
    ensureThreadId: () => (ensureUploadThreadId ? ensureUploadThreadId() : pane.ensureMaterializedThread()),
    getAttachmentCount: () => draft.attachments.length,
    addAttachment: (a, insertion) => imagePlaceholders.addUploadedAttachment(a, insertion),
    removeAttachment: (id) => draft.removeAttachment(id),
  });

  const imagePlaceholders = createComposerImagePlaceholders({
    getTextarea: () => textarea,
    getContent: () => draft.content,
    getAttachments: () => draft.attachments,
    setContentAndAttachments: (content, attachments) => draft.setContentAndAttachments(content, attachments),
    addAttachment: (attachment) => draft.addAttachment(attachment),
    removeAttachment: (id) => draft.removeAttachment(id),
    deleteAttachmentRecord: (id) => deleteAttachmentRecord(id),
    refreshTriggers: () => refreshCompletionTriggers(),
    autosizeTextarea,
    hasUserInputPrompt: () => !editsDraft,
  });

  // Accent-coloured command words (D31). Derived from the same value the
  // textarea renders, so they track every edit path — typing, completion,
  // paste, image-placeholder reconciliation — without a listener of its own.
  // Every occurrence is painted, because every one of them is live: the
  // backend expands the command once no matter how often the draft names it.
  // Suppressed during IME composition and while a selection is live; see
  // ComposerCommandHighlight for why each is a lie the overlay must not tell.
  let composingText = $state(false);
  let textareaScrollTop = $state(0);
  let selectionCollapsed = $state(true);
  // AO's registered words at any position, plus a leading intercepted command
  // (`/model`, `/clear`, …) — both are words AO acts on rather than sends, so
  // both read as commands. The intercepted one is painted only at position 0,
  // where interception actually fires. The two lists cannot overlap: no name
  // is in both registries.
  let commandRanges = $derived.by(() => {
    if (!editsDraft || disabled || composingText || !selectionCollapsed) return [];
    return [...slash.interceptedRanges(value), ...slashCommandMatches(value)];
  });

  $effect(() => {
    if (nativeAutosize) return;
    const next = value;
    const node = textarea;
    if (!node) return;
    if (lastAutosizedTextarea === node && lastAutosizedValue === next) return;
    queueMicrotask(() => {
      if (textarea === node && value === next) {
        autosizeTextarea();
      }
    });
  });

  function autosizeTextarea() {
    if (!textarea || nativeAutosize) return;
    textarea.style.height = 'auto';
    const measuredHeight = textarea.scrollHeight;
    if (measuredHeight > 0) {
      textarea.style.height = Math.min(measuredHeight, 200) + 'px';
    }
    lastAutosizedTextarea = textarea;
    lastAutosizedValue = value;
  }

  function deleteAttachmentRecord(id: string): void {
    if (shouldDeleteAttachmentRecord && !shouldDeleteAttachmentRecord(id)) return;
    void uploads.deleteAttachmentRecord(id);
  }

  function handleKeydown(event: KeyboardEvent) {
    dispatchComposerInputKeydown(event, {
      mentions,
      slash,
      claimKey: onKeydown,
      claimAfterPopovers: onKeydownAfterPopovers,
      placeholderKeydown: (e) => imagePlaceholders.handleAtomicPlaceholderKeydown(e),
      submitEnter: onSubmitEnter,
    });
  }

  function handleInput(event: Event) {
    const next = (event.target as HTMLTextAreaElement).value;
    // Reconciliation is a draft write of its own (content and attachment
    // list move together), so the host is told not to write over it.
    const appliedToDraft = editsDraft && imagePlaceholders.reconcileContent(next);
    oninput(next, { appliedToDraft });
    autosizeTextarea();
    slash.clearCommandError();
    refreshCompletionTriggers();
  }

  // Both completion menus read the same (value, caret) pair, so they refresh
  // together — a caret move that closes one must be able to open the other.
  function refreshCompletionTriggers() {
    mentions.refreshTriggers();
    slash.refreshTrigger();
    syncSelectionState();
  }

  // The command overlay hides while a selection covers text (its opaque word
  // would punch a hole in the highlight). Its scroll offset is read from the
  // textarea's own `scroll` event only: reading `scrollTop` here, on every
  // keystroke, forced a layout while the typed character had the textarea
  // dirty, and every scroll-offset change fires the event anyway (a height
  // collapse that clamps the offset included).
  function syncSelectionState() {
    if (!textarea) return;
    selectionCollapsed = textarea.selectionStart === textarea.selectionEnd;
  }

  function refuseAttachment(event: DragEvent | ClipboardEvent, notify = true): boolean {
    return blockAttachment?.(event, notify) ?? false;
  }

  function handlePaste(event: ClipboardEvent): void {
    if (refuseAttachment(event)) return;
    void uploads.handlePaste(event, imagePlaceholders.currentUploadInsertion());
  }

  function handleSelectionChange() {
    refreshCompletionTriggers();
  }

  function handleCompositionStart() {
    composingText = true;
  }

  function handleCompositionEnd() {
    composingText = false;
  }

  function handleTextareaScroll() {
    if (textarea) textareaScrollTop = textarea.scrollTop;
  }

  onDestroy(() => {
    mentions.closeMention();
  });

  // ---- handle (see ComposerInputSurfaceHandle) ----

  export function inputMounted(): boolean {
    return textarea !== undefined;
  }

  export function focusInputAtEnd(): void {
    if (textarea) focusTextareaAtEnd(textarea);
  }

  export function focusInputAtStart(): void {
    if (textarea) focusTextareaAt(textarea, 0);
  }

  export function inputSelection(): ComposerInputSelection | null {
    if (!textarea) return null;
    return { start: textarea.selectionStart, end: textarea.selectionEnd };
  }

  export function restoreInputSelection(selection: ComposerInputSelection): void {
    // Deliberately no focus() — see the handle's doc comment.
    // setSelectionRange clamps to the value's length itself, so a caret
    // captured against longer text lands at the end rather than throwing.
    textarea?.setSelectionRange(selection.start, selection.end);
  }

  export function resetInputHeight(): void {
    if (!textarea || nativeAutosize) return;
    textarea.style.height = 'auto';
  }

  let recreateScheduled = false;

  export function recreateInput(): void {
    // Deferred to an idle slot rather than run inside the caller's task: a
    // send's Enter keydown already carries the optimistic message mount, the
    // sidebar tier-move FLIP, and the dispatch RPC (8-19ms measured
    // 2026-08-27), and the swap adds ~2-3ms of teardown + refocus plus the
    // style recalc those force. The old element stays mounted and focused
    // until the swap task runs, so no frame ever renders without a textarea
    // or without focus either way.
    if (recreateScheduled) return;
    recreateScheduled = true;
    const fire = () => {
      recreateScheduled = false;
      performInputSwap();
    };
    if (typeof requestIdleCallback === 'function') {
      requestIdleCallback(fire, { timeout: 250 });
    } else {
      setTimeout(fire, 0);
    }
  }

  function performInputSwap(): void {
    const node = textarea;
    if (!node) return;
    // Mid-composition the IME holds uncommitted state on the element; a swap
    // would drop it. Likewise a user who resumed typing during the idle
    // window has live text (and possibly a mention/slash popup) anchored to
    // this element. Skip both — the next send catches the release.
    if (composingText || node.value !== '') return;
    const hadFocus = document.activeElement === node;
    inputEpoch += 1;
    // Flush the {#key} swap synchronously so the destroy, the mount, and the
    // conditional refocus all land in this task, before the next paint: no
    // frame ever renders without the textarea or without focus, and the
    // composer's focus-within styling never recalculates an unfocused state
    // (measured frame-by-frame, 2026-08-24). Focus is restored only when the
    // old element held it — a send clicked on the Send button keeps its
    // focus on the button, exactly as before.
    flushSync();
    if (hadFocus && textarea) focusTextareaAtEnd(textarea);
  }

  export function autosizeInput(): void {
    autosizeTextarea();
  }

  export function uploading(): boolean {
    return uploads.uploading;
  }

  export function handleDragEnter(event: DragEvent): void {
    if (refuseAttachment(event, false)) return;
    uploads.handleDragEnter(event);
  }

  export function handleDragOver(event: DragEvent): void {
    if (refuseAttachment(event, false)) return;
    uploads.handleDragOver(event);
  }

  export function handleDragLeave(event: DragEvent): void {
    uploads.handleDragLeave(event);
  }

  export function handleDrop(event: DragEvent): void {
    if (refuseAttachment(event)) return;
    void uploads.handleDrop(event, imagePlaceholders.currentUploadInsertion());
  }

  export function consumeInterceptedSend(message: string): boolean {
    return slash.consumeInterceptedSend(message);
  }

</script>

{#if showDraftRows}
  <ComposerAttachmentRow
    attachments={draft.attachments}
    onRemove={imagePlaceholders.removeAttachmentFromComposer}
    onExpand={onImageExpand}
    dragActive={uploads.dragActive}
    cache={attachmentCache}
  />
{/if}

<!-- Unlike the attachment row above, the chip row is always mounted and
     told whether to render: chip-expansion state lives inside it, so
     an {#if} here would drop what the reader opened every time an
     approval prompt takes the card. -->
<ComposerTerminalChipRow
  chips={draft.terminalChips}
  onRemove={draft.removeTerminalChip}
  visible={showDraftRows}
/>

<div class="px-4 pt-3 pb-2">
  <div class="relative">
    <ComposerMentionPopover
      anchor={textarea}
      open={mentions.mentionTrigger !== null}
      query={mentions.mentionTrigger?.query ?? ''}
      results={mentions.mentionResults}
      activeIndex={mentions.mentionActiveIndex}
      loading={mentions.mentionLoading}
      workspacePath={paneWorkspacePath(pane)}
      onSelect={mentions.insertMention}
      onClose={mentions.closeMention}
      onHover={(idx) => mentions.setMentionActiveIndex(idx)}
    />

    <ComposerSlashPopover
      anchor={textarea}
      open={slash.slashOpen}
      sections={slash.slashSections}
      activeIndex={slash.slashActiveIndex}
      onSelect={slash.insertCommand}
      onClose={slash.closeSlash}
      onHover={(idx) => slash.setSlashActiveIndex(idx)}
    />

    {#key inputEpoch}
    <textarea
      bind:this={textarea}
      onbeforeinput={imagePlaceholders.handleBeforeInput}
      onkeydown={handleKeydown}
      oninput={handleInput}
      onselect={handleSelectionChange}
      onkeyup={handleSelectionChange}
      onclick={handleSelectionChange}
      oncompositionstart={handleCompositionStart}
      oncompositionend={handleCompositionEnd}
      onscroll={handleTextareaScroll}
      onpaste={handlePaste}
      {disabled}
      {placeholder}
      aria-label="Message Input"
      rows={1}
      {value}
      class="field-sizing-content max-h-50 w-full resize-none bg-transparent px-1 py-1 text-[0.8125rem] leading-[1.55] text-fg placeholder:text-fg-hint focus:outline-none disabled:opacity-40 disabled:cursor-not-allowed"
    ></textarea>
    {/key}

    <!-- After the textarea in DOM order as well as above it in paint
         order, so each accent word covers the glyphs it replaces. -->
    <ComposerCommandHighlight
      {value}
      ranges={commandRanges}
      scrollTop={textareaScrollTop}
      {textarea}
    />
  </div>

</div>

{#if showDraftRows && slash.commandError}
  <!-- Composer-local, not a toast and not the pane banner: the user is
       looking at the text they just typed, which is where the answer to
       "that command did not work" belongs. Cleared by the next edit. -->
  <div
    class="px-4 pb-1 text-[0.6875rem] text-error"
    role="alert"
    data-testid="composer-command-error"
  >
    {slash.commandError}
  </div>
{/if}
