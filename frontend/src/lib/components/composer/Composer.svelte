<script lang="ts">
  // Pure message entry. Coordinates between the draft store, the mention +
  // slash popovers (composerMentions.svelte.ts), and the upload flow
  // (composerUploads.svelte.ts). Everything else — model/provider picker,
  // effort + fast-mode, runtime mode, mode cycle, branch picker, env /
  // worktree picker — lives in the composer toolbar / below-composer bar.

  import { onDestroy } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import ComposerAttachmentRow from './ComposerAttachmentRow.svelte';
  import ComposerMentionPopover from './ComposerMentionPopover.svelte';
  import ComposerSlashPopover from './ComposerSlashPopover.svelte';
  import ComposerTerminalChip from './ComposerTerminalChip.svelte';
  import ComposerToolbar from './toolbar/ComposerToolbar.svelte';
  import { handleMentionPopoverKeydown } from './composerKeyboard';
  import { createComposerMentions } from './composerMentions.svelte';
  import { createComposerUploads } from './composerUploads.svelte';
  import { dispatchInterrupt, dispatchSend } from './composerSend';

  interface Props {
    pane: ThreadPane;
    draft: ComposerDraftStore;
  }

  let { pane, draft }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state(undefined);
  let expandedChips = new Set<string>();
  let expandedVersion = $state(0);

  const mentions = createComposerMentions({
    getTextarea: () => textarea,
    getThreadId: () => pane.threadId,
    setContent: (value) => draft.setContent(value),
  });

  const uploads = createComposerUploads({
    getThreadId: () => pane.threadId,
    addAttachment: (a) => draft.addAttachment(a),
    removeAttachment: (id) => draft.removeAttachment(id),
  });

  let isDisabled = $derived(!pane.threadId);
  // Mid-turn guard: block sends while a turn is in flight (any streaming text,
  // any running tool, or an optimistic pending message). The user must press
  // Interrupt first. Editing and uploading stay enabled so the next message can
  // be prepared in advance.
  let isTurnActive = $derived(pane.isTurnActive);
  let sending = $state(false);
  let hasDraftContent = $derived(
    draft.content.trim().length > 0 ||
      draft.attachments.length > 0 ||
      draft.terminalChips.length > 0,
  );
  let canSend = $derived(!isDisabled && !isTurnActive && !sending && hasDraftContent);
  // Polite aria-live error raised when the user hits Enter during an active
  // turn. Cleared when the turn ends or the user types a new character so it
  // doesn't re-announce on every subsequent keystroke.
  let midTurnBlockMessage: string = $state('');

  $effect(() => {
    if (!isTurnActive) midTurnBlockMessage = '';
  });

  // Reset slash cache when the pane's thread changes. Pane-scoped state lives
  // inside the mentions module; this $effect is the single hook.
  $effect(() => {
    mentions.onThreadChanged(pane.thread?.id ?? null);
  });

  async function send() {
    if (!pane.threadId || !canSend) return;
    midTurnBlockMessage = '';
    const message = draft.composeOutgoingMessage();
    sending = true;

    const threadId = pane.threadId;
    // Capture the pre-send draft contents bound to THIS thread. If the user
    // switches threads before SendMessage resolves and the send rejects, we
    // must not bleed the snapshot into the new pane's local composer.
    const snapshot = {
      content: draft.content,
      attachments: draft.attachments.slice(),
      terminalChips: draft.terminalChips.slice(),
    };

    draft.setContent('');
    await draft.clearAfterSend();
    resetTextareaHeight();

    try {
      await dispatchSend({
        threadId,
        message,
        snapshot,
        currentThread: pane.thread,
        restoreDraft: (tid, snap) => draft.restoreDraftFor(tid, snap),
        draftThreadId: () => draft.threadId,
        reportError: (msg) => pane.setGeneralError(msg),
      });
    } finally {
      sending = false;
    }
  }

  async function interrupt() {
    if (!pane.threadId) return;
    await dispatchInterrupt(pane.threadId, (msg) => pane.setGeneralError(msg));
    midTurnBlockMessage = '';
  }

  function resetTextareaHeight() {
    if (!textarea) return;
    textarea.style.height = 'auto';
  }

  function autosizeTextarea() {
    if (!textarea) return;
    textarea.style.height = 'auto';
    textarea.style.height = Math.min(textarea.scrollHeight, 200) + 'px';
  }

  async function handleKeydown(e: KeyboardEvent) {
    // Shift+Tab globally cycles thread mode. Prevent the browser's outdent
    // and let the App-level keydown fire. User has confirmed textarea
    // outdent is not wanted.
    if (e.key === 'Tab' && e.shiftKey && !mentions.mentionTrigger && !mentions.slashTrigger) {
      e.preventDefault();
      return;
    }

    // Popover dispatch (mention + slash) short-circuits when the keystroke
    // was consumed; otherwise we fall through to the send guard below.
    if (handleMentionPopoverKeydown(e, mentions)) return;

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      if (isTurnActive) {
        midTurnBlockMessage = 'Cannot send during an active turn. Press Interrupt first.';
        return;
      }
      await send();
    }
  }

  function handleInput(event: Event) {
    const value = (event.target as HTMLTextAreaElement).value;
    draft.setContent(value);
    autosizeTextarea();
    mentions.refreshTriggers();
    if (midTurnBlockMessage) midTurnBlockMessage = '';
  }

  function handleSelectionChange() {
    mentions.refreshTriggers();
  }

  function handleToggleChip(id: string) {
    if (expandedChips.has(id)) {
      expandedChips.delete(id);
    } else {
      expandedChips.add(id);
    }
    expandedVersion++;
  }

  function isChipExpanded(id: string): boolean {
    void expandedVersion;
    return expandedChips.has(id);
  }

  onDestroy(() => {
    mentions.closeMention();
    mentions.closeSlash();
  });
</script>

<div
  class="relative px-6 pb-4 pt-1"
  ondragenter={uploads.handleDragEnter}
  ondragover={uploads.handleDragOver}
  ondragleave={uploads.handleDragLeave}
  ondrop={uploads.handleDrop}
  role="region"
  aria-label="Message composer"
  data-testid="composer-root"
>
  <div
    class="mx-auto w-full max-w-[52rem] rounded-[var(--radius-composer)] border border-border-subtle bg-card/70 backdrop-blur-sm shadow-sheet overflow-hidden
           focus-within:border-border focus-within:shadow-menu transition-[border-color,box-shadow] duration-200"
  >
    <ComposerAttachmentRow
      attachments={draft.attachments}
      onRemove={uploads.removeAttachment}
      dragActive={uploads.dragActive}
    />

    {#if draft.terminalChips.length > 0}
      <div
        class="flex flex-col gap-1 border-b border-border-subtle bg-surface-0/40 px-4 py-2"
        data-testid="terminal-chip-row"
      >
        {#each draft.terminalChips as chip (chip.id)}
          <ComposerTerminalChip
            {chip}
            expanded={isChipExpanded(chip.id)}
            onToggle={handleToggleChip}
            onRemove={draft.removeTerminalChip}
          />
        {/each}
      </div>
    {/if}

    {#if isTurnActive}
      <div
        class="flex items-center gap-2 border-b border-border-subtle bg-accent/10 px-4 py-1.5 text-[11px] text-fg-muted"
        role="status"
        aria-live="polite"
        data-testid="composer-turn-banner"
      >
        <span class="h-1.5 w-1.5 animate-pulse rounded-full bg-accent shrink-0" aria-hidden="true"></span>
        <span class="truncate">Agent is responding.</span>
      </div>
    {/if}

    <div class="px-4 pt-3 pb-2">
      <div class="relative">
        <ComposerMentionPopover
          anchor={textarea}
          open={mentions.mentionTrigger !== null}
          query={mentions.mentionTrigger?.query ?? ''}
          results={mentions.mentionResults}
          activeIndex={mentions.mentionActiveIndex}
          loading={mentions.mentionLoading}
          onSelect={mentions.insertMention}
          onClose={mentions.closeMention}
          onHover={(idx) => mentions.setMentionActiveIndex(idx)}
        />

        <ComposerSlashPopover
          anchor={textarea}
          open={mentions.slashTrigger !== null}
          query={mentions.slashTrigger?.text ?? ''}
          commands={mentions.slashFilteredCommands}
          activeIndex={mentions.slashActiveIndex}
          onSelect={mentions.insertSlashCommand}
          onClose={mentions.closeSlash}
          onHover={(idx) => mentions.setSlashActiveIndex(idx)}
        />

        <textarea
          bind:this={textarea}
          value={draft.content}
          onkeydown={handleKeydown}
          oninput={handleInput}
          onselect={handleSelectionChange}
          onkeyup={handleSelectionChange}
          onclick={handleSelectionChange}
          onpaste={uploads.handlePaste}
          disabled={isDisabled}
          placeholder={isDisabled
            ? 'Select or create a thread to start'
            : 'Send a message… (Shift+Enter for newline, @ to mention a file)'}
          aria-label="Message input"
          rows={1}
          class="w-full resize-none bg-transparent px-1 py-1 text-[13px] leading-[1.55] text-fg placeholder:text-fg-hint focus:outline-none disabled:opacity-40 disabled:cursor-not-allowed"
        ></textarea>
      </div>

      {#if midTurnBlockMessage}
        <div
          class="mt-1 text-[11px] text-error/90"
          role="alert"
          aria-live="polite"
          data-testid="composer-midturn-error"
        >
          {midTurnBlockMessage}
        </div>
      {:else}
        <div
          class="sr-only"
          role="alert"
          aria-live="polite"
          data-testid="composer-midturn-error"
        ></div>
      {/if}
    </div>

    <ComposerToolbar
      {pane}
      {canSend}
      {isTurnActive}
      onSend={send}
      onInterrupt={interrupt}
    />
  </div>
</div>
