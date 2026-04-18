<script lang="ts">
  import { onDestroy } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    DeleteAttachment,
    GetThreadSlashCommands,
    InterruptTurn,
    SearchWorkspaceFiles,
    SendMessage,
    UploadAttachment,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import type { Attachment } from '../../types/attachment';
  import {
    DEFAULT_MAX_ATTACHMENT_SIZE,
    isAllowedAttachmentMime,
  } from '../../types/attachment';
  import type { WorkspaceFile, WorkspaceFileSearchResult } from '../../types/workspaceFile';
  import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
  import { applyMention, detectMentionTrigger, type MentionTrigger } from './mentionHelpers';
  import {
    applySlashCommand,
    detectSlashTrigger,
    type SlashTrigger,
  } from './slashHelpers';
  import ComposerAttachmentRow from './ComposerAttachmentRow.svelte';
  import ComposerMentionPopover from './ComposerMentionPopover.svelte';
  import ComposerSlashPopover from './ComposerSlashPopover.svelte';
  import ComposerTerminalChip from './ComposerTerminalChip.svelte';

  interface Props {
    pane: ThreadPane;
    draft: ComposerDraftStore;
  }

  let { pane, draft }: Props = $props();

  let textarea: HTMLTextAreaElement | undefined = $state(undefined);
  let dragDepth = $state(0);
  let mentionTrigger: MentionTrigger | null = $state(null);
  let mentionResults: WorkspaceFile[] = $state([]);
  let mentionActiveIndex = $state(0);
  let mentionLoading = $state(false);
  let mentionSearchGeneration = 0;
  // Slash-command popover: Claude surfaces user-configurable commands via
  // system.init; the cache on the Go side is refreshed per init and fetched
  // once per thread here. Codex threads get an empty list and the popover
  // shows the "no commands available" empty state.
  let slashTrigger: SlashTrigger | null = $state(null);
  let slashCommandsCache: string[] = $state([]);
  let slashActiveIndex = $state(0);
  let slashFetchedForThread: string | null = null;
  let expandedChips = new Set<string>();
  let expandedVersion = $state(0);

  const MAX_ATTACHMENT_SIZE = DEFAULT_MAX_ATTACHMENT_SIZE;

  let isDisabled = $derived(!pane.threadId);
  // Derived view over the slash-command cache, filtered by the active trigger
  // text. Pure function of cache + trigger so no $effect is needed.
  let slashFilteredCommands = $derived.by(() => {
    if (!slashTrigger) return [] as string[];
    const q = slashTrigger.text.toLowerCase();
    if (!q) return slashCommandsCache.slice();
    return slashCommandsCache.filter((cmd) => cmd.toLowerCase().includes(q));
  });
  // Mid-turn guard: block sends while a turn is in flight (any streaming text,
  // any running tool, or an optimistic pending message). The user must press
  // Interrupt first. Editing and uploading stay enabled so the next message can
  // be prepared in advance.
  let isTurnActive = $derived(pane.isTurnActive);
  let hasDraftContent = $derived(
    draft.content.trim().length > 0 ||
      draft.attachments.length > 0 ||
      draft.terminalChips.length > 0,
  );
  let canSend = $derived(!isDisabled && !isTurnActive && hasDraftContent);
  let dragActive = $derived(dragDepth > 0);
  // Polite aria-live error raised when the user hits Enter during an active
  // turn. Cleared when the turn ends or the user types a new character so it
  // doesn't re-announce on every subsequent keystroke.
  let midTurnBlockMessage: string = $state('');

  $effect(() => {
    if (!isTurnActive) midTurnBlockMessage = '';
  });

  async function send() {
    if (!pane.threadId || !canSend) return;
    midTurnBlockMessage = '';
    const message = draft.composeOutgoingMessage();
    pane.setPendingMessage(message);

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
      await SendMessage(threadId, message);
    } catch (err) {
      console.error('Failed to send message:', err);
      pane.setPendingMessage(null);
      // restoreDraftFor always persists to the captured thread; it only
      // touches local UI state when the draft store is still on that thread.
      // If the user has moved on, surface a toast so the failed send is
      // visible rather than silent.
      await draft.restoreDraftFor(threadId, snapshot);
      if (draft.threadId !== threadId) {
        addToast(
          'error',
          `Message to the previous thread failed to send; draft preserved (${err}).`,
        );
      } else {
        pane.setError(`Failed to send message: ${err}`);
      }
    }
  }

  async function interrupt() {
    if (!pane.threadId) return;
    try {
      await InterruptTurn(pane.threadId);
      midTurnBlockMessage = '';
    } catch (err) {
      console.error('Failed to interrupt turn:', err);
      pane.setError(`Failed to interrupt: ${err}`);
    }
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
    if (mentionTrigger) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        mentionActiveIndex = Math.min(
          mentionActiveIndex + 1,
          Math.max(0, mentionResults.length - 1),
        );
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        mentionActiveIndex = Math.max(mentionActiveIndex - 1, 0);
        return;
      }
      if ((e.key === 'Enter' || e.key === 'Tab') && mentionResults[mentionActiveIndex]) {
        e.preventDefault();
        insertMention(mentionResults[mentionActiveIndex]);
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        closeMention();
        return;
      }
    }

    if (slashTrigger) {
      // The two popovers are mutually exclusive — only one trigger is open
      // at a time (see refreshTriggers) — so the ordering between the
      // mention and slash guard blocks doesn't produce a conflict.
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        slashActiveIndex = Math.min(
          slashActiveIndex + 1,
          Math.max(0, slashFilteredCommands.length - 1),
        );
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        slashActiveIndex = Math.max(slashActiveIndex - 1, 0);
        return;
      }
      if (
        (e.key === 'Enter' || e.key === 'Tab') &&
        slashFilteredCommands[slashActiveIndex]
      ) {
        e.preventDefault();
        insertSlashCommand(slashFilteredCommands[slashActiveIndex]);
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        closeSlash();
        return;
      }
    }

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      if (isTurnActive) {
        // Mid-turn guard: announce the block politely so a screen-reader user
        // knows why nothing happened. The message clears when the turn ends or
        // the user types something new.
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
    refreshTriggers();
    if (midTurnBlockMessage) midTurnBlockMessage = '';
  }

  // refreshTriggers inspects the textarea for both an active @mention trigger
  // and a start-of-message /slash trigger. Only one popover can be open at a
  // time; the slash trigger only fires at index 0 (first character of the
  // message), so a message that starts with `@` and a later `/` will only
  // ever show the mention popover — the two rules don't overlap in practice.
  function refreshTriggers() {
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
      // Filtering is derived; just clamp the active index when the list
      // shrinks so the highlighted row stays in range.
      if (slashActiveIndex >= slashFilteredCommands.length) {
        slashActiveIndex = 0;
      }
      void ensureSlashCommandsLoaded();
      return;
    }

    closeMention();
    closeSlash();
  }

  function refreshMentionTrigger() {
    // Preserved as a thin alias for clarity at the remaining call sites
    // (selection change). Both triggers refresh together.
    refreshTriggers();
  }

  async function loadMentionResults(query: string) {
    if (!pane.threadId) {
      mentionResults = [];
      return;
    }
    const generation = ++mentionSearchGeneration;
    mentionLoading = true;
    try {
      const result = (await SearchWorkspaceFiles(
        pane.threadId,
        query,
        50,
      )) as WorkspaceFileSearchResult;
      if (generation !== mentionSearchGeneration) return;
      mentionResults = result?.files ?? [];
      mentionActiveIndex = 0;
    } catch (err) {
      if (generation !== mentionSearchGeneration) return;
      console.error('SearchWorkspaceFiles failed:', err);
      mentionResults = [];
      addToast('warning', `Workspace search failed: ${err}`);
    } finally {
      if (generation === mentionSearchGeneration) {
        mentionLoading = false;
      }
    }
  }

  function insertMention(file: WorkspaceFile) {
    if (!mentionTrigger || !textarea) return;
    const current = textarea.value;
    const { value, caret } = applyMention(current, mentionTrigger, file.path);
    draft.setContent(value);
    textarea.value = value;
    requestAnimationFrame(() => {
      if (!textarea) return;
      textarea.focus();
      textarea.setSelectionRange(caret, caret);
      autosizeTextarea();
    });
    closeMention();
  }

  function closeMention() {
    mentionTrigger = null;
    mentionResults = [];
    mentionActiveIndex = 0;
    mentionSearchGeneration++;
  }

  async function ensureSlashCommandsLoaded() {
    const threadId = pane.threadId;
    if (!threadId) {
      slashCommandsCache = [];
      slashFetchedForThread = null;
      return;
    }
    if (slashFetchedForThread === threadId) return;
    slashFetchedForThread = threadId;
    try {
      const result = (await GetThreadSlashCommands(threadId)) as string[];
      // Guard against a thread switch in flight: only apply the result if the
      // binding's thread matches the current pane's thread.
      if (pane.threadId !== threadId) return;
      slashCommandsCache = Array.isArray(result) ? result : [];
      if (slashActiveIndex >= slashCommandsCache.length) {
        slashActiveIndex = 0;
      }
    } catch (err) {
      console.error('GetThreadSlashCommands failed:', err);
      // Leave the cache untouched; the popover will fall back to its empty
      // state. Repeat fetches are skipped because `slashFetchedForThread` is
      // now set — we'd rather stay quiet than spam the binding on every
      // keystroke.
      if (pane.threadId === threadId) {
        slashCommandsCache = [];
      }
    }
  }

  function insertSlashCommand(command: string) {
    if (!slashTrigger || !textarea) return;
    const current = textarea.value;
    const { value, nextCaret } = applySlashCommand(current, slashTrigger, command);
    draft.setContent(value);
    textarea.value = value;
    requestAnimationFrame(() => {
      if (!textarea) return;
      textarea.focus();
      textarea.setSelectionRange(nextCaret, nextCaret);
      autosizeTextarea();
    });
    closeSlash();
  }

  function closeSlash() {
    slashTrigger = null;
    slashActiveIndex = 0;
  }

  // When the pane's thread changes, reset the cache + the fetched marker so
  // the next slash trigger refetches against the new thread. The cache is
  // intentionally cleared so a stale list from the previous thread doesn't
  // flash for a frame while the fetch is in flight.
  $effect(() => {
    const id = pane.thread?.id ?? null;
    if (id !== slashFetchedForThread) {
      slashCommandsCache = [];
      slashFetchedForThread = null;
      closeSlash();
    }
  });

  function handleSelectionChange() {
    refreshMentionTrigger();
  }

  async function uploadFiles(files: FileList | File[]) {
    if (!pane.threadId) return;
    const threadId = pane.threadId;
    const list = Array.from(files);
    for (const file of list) {
      await uploadOne(threadId, file);
    }
  }

  async function uploadOne(threadId: string, file: File) {
    if (!isAllowedAttachmentMime(file.type) && !matchesImageExtension(file.name)) {
      addToast('warning', `Unsupported file type: ${file.name}`);
      return;
    }
    if (file.size > MAX_ATTACHMENT_SIZE) {
      addToast(
        'warning',
        `${file.name} is ${(file.size / 1024 / 1024).toFixed(1)} MB; limit is ${Math.round(
          MAX_ATTACHMENT_SIZE / 1024 / 1024,
        )} MB`,
      );
      return;
    }
    try {
      const base64 = await fileToBase64(file);
      const record = (await UploadAttachment(
        threadId,
        file.name,
        file.type || '',
        base64,
      )) as Attachment;
      if (pane.threadId === threadId) {
        draft.addAttachment(record);
      }
    } catch (err) {
      console.error('UploadAttachment failed:', err);
      addToast('error', `Upload failed: ${err}`);
    }
  }

  function matchesImageExtension(name: string): boolean {
    return /\.(png|jpe?g|gif|webp)$/i.test(name);
  }

  function fileToBase64(file: File): Promise<string> {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onerror = () => reject(new Error('Failed to read file'));
      reader.onload = () => {
        const result = reader.result;
        if (typeof result !== 'string') {
          reject(new Error('Unexpected reader result'));
          return;
        }
        const commaIdx = result.indexOf(',');
        resolve(commaIdx >= 0 ? result.slice(commaIdx + 1) : result);
      };
      reader.readAsDataURL(file);
    });
  }

  function handleDragEnter(event: DragEvent) {
    if (!hasImagePayload(event)) return;
    event.preventDefault();
    dragDepth += 1;
  }

  function handleDragLeave(_event: DragEvent) {
    if (dragDepth > 0) {
      dragDepth -= 1;
    }
  }

  function handleDragOver(event: DragEvent) {
    if (!hasImagePayload(event)) return;
    event.preventDefault();
  }

  async function handleDrop(event: DragEvent) {
    dragDepth = 0;
    if (!event.dataTransfer) return;
    const files = event.dataTransfer.files;
    if (!files || files.length === 0) return;
    event.preventDefault();
    await uploadFiles(files);
  }

  function hasImagePayload(event: DragEvent): boolean {
    const types = event.dataTransfer?.types;
    if (!types) return false;
    return Array.from(types).some((type) => type === 'Files' || type.startsWith('image/'));
  }

  async function handlePaste(event: ClipboardEvent) {
    const clip = event.clipboardData;
    if (!clip) return;
    const files: File[] = [];
    for (const item of Array.from(clip.items)) {
      if (item.kind === 'file' && item.type.startsWith('image/')) {
        const file = item.getAsFile();
        if (file) files.push(file);
      }
    }
    if (files.length === 0) return;
    event.preventDefault();
    await uploadFiles(files);
  }

  async function handleRemoveAttachment(id: string) {
    draft.removeAttachment(id);
    try {
      await DeleteAttachment(id);
    } catch (err) {
      console.error('DeleteAttachment failed:', err);
      addToast('warning', `Failed to delete attachment: ${err}`);
    }
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
    closeMention();
    closeSlash();
  });
</script>

<div
  class="relative border-t border-border bg-surface-1"
  ondragenter={handleDragEnter}
  ondragover={handleDragOver}
  ondragleave={handleDragLeave}
  ondrop={handleDrop}
  role="region"
  aria-label="Message composer"
  data-testid="composer-root"
>
  <ComposerAttachmentRow
    attachments={draft.attachments}
    onRemove={handleRemoveAttachment}
    {dragActive}
  />

  {#if draft.terminalChips.length > 0}
    <div
      class="flex flex-col gap-1 border-b border-border bg-surface-0 px-4 py-2"
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
      class="flex items-center gap-2 border-b border-border bg-accent/10 px-4 py-1.5 text-xs text-text-secondary"
      role="status"
      aria-live="polite"
      data-testid="composer-turn-banner"
    >
      <span class="h-2 w-2 animate-pulse rounded-full bg-accent shrink-0" aria-hidden="true"></span>
      <span class="truncate">Agent is responding.</span>
      <button
        type="button"
        onclick={interrupt}
        data-testid="composer-interrupt"
        aria-label="Interrupt the current turn"
        class="ml-auto shrink-0 rounded border border-error/50 bg-error/15 px-2 py-0.5 text-error hover:bg-error/25 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-error/50"
      >
        Interrupt
      </button>
    </div>
  {/if}

  <div class="px-4 py-3">
    <div class="relative flex gap-2 items-end">
      <ComposerMentionPopover
        open={mentionTrigger !== null}
        query={mentionTrigger?.query ?? ''}
        results={mentionResults}
        activeIndex={mentionActiveIndex}
        loading={mentionLoading}
        onSelect={insertMention}
        onHover={(idx) => (mentionActiveIndex = idx)}
      />

      <ComposerSlashPopover
        open={slashTrigger !== null}
        query={slashTrigger?.text ?? ''}
        commands={slashFilteredCommands}
        activeIndex={slashActiveIndex}
        onSelect={insertSlashCommand}
        onHover={(idx) => (slashActiveIndex = idx)}
      />

      <textarea
        bind:this={textarea}
        value={draft.content}
        onkeydown={handleKeydown}
        oninput={handleInput}
        onselect={handleSelectionChange}
        onkeyup={handleSelectionChange}
        onclick={handleSelectionChange}
        onpaste={handlePaste}
        disabled={isDisabled}
        placeholder={isDisabled
          ? 'Select or create a thread to start'
          : 'Send a message... (Shift+Enter for newline, @ to mention a file)'}
        aria-label="Message input"
        rows={1}
        class="flex-1 resize-none rounded-lg border border-border bg-surface-0 px-3 py-2.5 text-sm text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
      ></textarea>

      <button
        onclick={send}
        disabled={!canSend}
        data-testid="composer-send"
        title={isTurnActive ? 'Agent is responding. Press Interrupt to cancel.' : undefined}
        aria-label="Send message"
        class="shrink-0 rounded-lg px-4 py-2.5 text-sm font-medium bg-accent text-surface-0 hover:bg-accent/85 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Send
      </button>
    </div>

    <div
      class="mt-1 min-h-[1rem] text-xs text-error/90"
      role="alert"
      aria-live="polite"
      data-testid="composer-midturn-error"
    >
      {midTurnBlockMessage}
    </div>
  </div>
</div>
