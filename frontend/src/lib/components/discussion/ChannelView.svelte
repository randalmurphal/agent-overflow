<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import type { ChannelMessage } from '../../types/discussion';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { GetChannelMessages, PostChannelMessage } from '../../stores/bindings';
  import { getSettings } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { createStickToBottomController } from '../../utils/stickToBottom.svelte';
  import { relativeTime } from '../../utils/format';
  import Button from '../primitives/Button.svelte';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ScrollToBottomButton from '../chat/ScrollToBottomButton.svelte';

  let {
    pane,
    channelId,
  }: {
    pane: ThreadPane;
    channelId: string;
  } = $props();

  const POLL_INTERVAL_MS = 2500;
  const POLL_MAX_INTERVAL_MS = 40_000;
  const PAGE_LIMIT = 200;

  let pollGeneration = 0;
  let pollTimer: ReturnType<typeof setTimeout> | null = null;
  // Consecutive-failure counter that drives exponential backoff. Reset
  // to 0 on any successful poll. Without this, a persistently-erroring
  // backend would keep hammering GetChannelMessages every 2.5s with
  // the error banner stuck on screen.
  let consecutiveErrors = 0;
  let lastChannelId = '';
  let composing = $state('');
  let posting = $state(false);
  let pollError = $state<string | null>(null);
  let loadingInitial = $state(true);
  let scrollContainer: HTMLDivElement | undefined = $state(undefined);
  const stickToBottom = createStickToBottomController({
    getContainer: () => scrollContainer,
  });

  let messages = $derived(pane.channelMessages);
  let status = $derived(pane.channelStatus ?? 'open');
  let concluded = $derived(status === 'concluded' || status === 'closed');
  let canPost = $derived(!concluded && composing.trim().length > 0 && !posting);

  $effect(() => {
    // Only re-run when the channelId prop changes; don't subscribe to the pane
    // state that pollOnce reads/writes, or we loop on every new message.
    if (!channelId) return;
    untrack(() => {
      // Only wipe the channel buffer when we're switching to a different
      // channel — re-entry of the same channel preserves whatever status
      // the caller pre-seeded (e.g. `concluded` after a prior run).
      if (lastChannelId !== channelId) {
        pane.clearChannel();
        lastChannelId = channelId;
      }
      loadingInitial = true;
      pollError = null;
      const generation = ++pollGeneration;
      void pollOnce(generation, /*initial*/ true);
    });

    return () => {
      // Bump generation to cancel any in-flight polls for this channel.
      pollGeneration++;
      if (pollTimer) {
        clearTimeout(pollTimer);
        pollTimer = null;
      }
    };
  });

  onDestroy(() => {
    pollGeneration++;
    if (pollTimer) {
      clearTimeout(pollTimer);
      pollTimer = null;
    }
    stickToBottom.destroy();
  });

  async function pollOnce(generation: number, initial: boolean): Promise<void> {
    if (generation !== pollGeneration) return;
    const currentMessages = pane.channelMessages;
    const afterSeq = currentMessages.length > 0
      ? currentMessages[currentMessages.length - 1].sequence
      : 0;
    try {
      const raw = await GetChannelMessages(channelId, afterSeq, PAGE_LIMIT);
      if (generation !== pollGeneration) return;
      const incoming = (raw ?? []) as ChannelMessage[];
      if (incoming.length > 0) {
        pane.mergeChannelMessages(incoming);
      }
      pollError = null;
      consecutiveErrors = 0;
      // Seed status to `open` on first successful load. The backend flips to
      // `concluded` via DB, not via an event, so status is inferred from the
      // deliberation engine's MaxTurns behavior and the Channel row — if we
      // want authoritative status we'd need a new binding. For now: seeded
      // `open` and stays so unless explicitly overridden.
      if (pane.channelStatus === null) {
        pane.setChannelStatus('open');
      }
    } catch (err) {
      if (generation !== pollGeneration) return;
      console.error('Channel poll failed:', err);
      pollError = errString(err);
      consecutiveErrors += 1;
    } finally {
      if (generation === pollGeneration) {
        if (initial) {
          loadingInitial = false;
        }
        // Exponential backoff on consecutive failures so a stuck backend
        // doesn't spam the binding every 2.5s. Cap at 40s — long enough
        // to notice persistent errors, short enough that the UI recovers
        // quickly when the backend comes back.
        const delay = Math.min(
          POLL_INTERVAL_MS * 2 ** consecutiveErrors,
          POLL_MAX_INTERVAL_MS,
        );
        pollTimer = setTimeout(() => pollOnce(generation, false), delay);
      }
    }
  }

  // Attach the controller's listeners once the scroll container is bound.
  // `void` makes the reactive read explicit so the compiler tracks the
  // dependency reliably across Svelte versions.
  $effect(() => {
    void scrollContainer;
    stickToBottom.attach();
  });

  $effect(() => {
    // Notify the controller whenever the message list grows. The
    // controller decides whether to actually scroll based on intent
    // (sticky vs free), pause leases, and pointer state.
    const len = messages.length;
    if (len === 0) return;
    untrack(() => stickToBottom.notifyContentMaybeGrew());
  });

  async function handlePost(): Promise<void> {
    const content = composing.trim();
    if (!content || posting || concluded) return;
    posting = true;
    const savedText = composing;
    composing = '';
    // Posting is an explicit "I want to follow this conversation" signal —
    // re-arm stickiness even if the user had scrolled up.
    stickToBottom.forceStick();
    try {
      await PostChannelMessage(channelId, content);
      // Immediate poll to surface our message rather than wait for interval.
      void pollOnce(pollGeneration, false);
    } catch (err) {
      console.error('Failed to post channel message:', err);
      composing = savedText;
      addToast('error', `Failed to post message: ${errString(err)}`);
    } finally {
      posting = false;
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handlePost();
    }
  }

  function messageAccentClass(msg: ChannelMessage): string {
    if (msg.fromType === 'human') {
      return 'border-accent/25 bg-accent/5';
    }
    return 'border-border-subtle bg-card/30';
  }

  function roleLabel(msg: ChannelMessage): string {
    if (msg.fromType === 'human') return 'You';
    return msg.fromRole?.trim() || 'agent';
  }

  function roleBadgeClass(msg: ChannelMessage): string {
    if (msg.fromType === 'human') {
      return 'bg-accent/10 text-accent border-accent/25';
    }
    return 'bg-surface-2/40 text-fg-muted border-border-subtle';
  }

  function statusLabel(): string {
    if (concluded) return 'Concluded';
    return 'Live';
  }
</script>

<div class="flex h-full flex-col min-h-0">
  <div class="border-b border-border-subtle px-5 py-2 flex items-center gap-3 shrink-0">
    <span class="inline-flex items-center gap-1.5 rounded-[var(--radius-field)] border px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide
      {concluded ? 'border-border-subtle bg-surface-2/40 text-fg-muted' : 'border-success/30 bg-success/10 text-success'}">
      <span class="w-1.5 h-1.5 rounded-full {concluded ? 'bg-fg-subtle' : 'bg-success'}" aria-hidden="true"></span>
      {statusLabel()}
    </span>
    <span class="text-[11px] text-fg-muted tabular-nums">
      {messages.length} {messages.length === 1 ? 'message' : 'messages'}
    </span>
    {#if pollError}
      <span role="alert" class="ml-auto text-[11px] text-error truncate max-w-[280px]" title={pollError}>
        Poll error: {pollError}
      </span>
    {/if}
  </div>

  <div class="relative flex-1 min-h-0 flex flex-col">
    <div
      bind:this={scrollContainer}
      class="flex-1 min-h-0 overflow-y-auto px-5 py-4 space-y-3"
      role="log"
      aria-live="polite"
      aria-label="Discussion Channel Messages"
      data-testid="channel-message-list"
    >
      {#if loadingInitial}
        <div class="text-[12px] text-fg-subtle">Loading channel messages…</div>
      {:else if messages.length === 0}
        <div class="text-[12px] text-fg-subtle">
          No messages yet. Participants will begin speaking as their turns complete.
        </div>
      {:else}
        {#each messages as msg (msg.id || msg.sequence)}
          <div class="rounded-[var(--radius-card)] border {messageAccentClass(msg)} px-3.5 py-2.5">
            <div class="flex items-center gap-2 mb-1.5">
              <span class="rounded-[var(--radius-field)] border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-[0.14em] {roleBadgeClass(msg)}">
                {roleLabel(msg)}
              </span>
              <span class="text-[11px] text-fg-hint tabular-nums">#{msg.sequence}</span>
              <span class="ml-auto text-[11px] text-fg-hint">
                {relativeTime(msg.createdAt, getSettings().timestampFormat)}
              </span>
            </div>
            <ChatMarkdown source={msg.content} class="text-[13px] text-fg break-words" />
          </div>
        {/each}
      {/if}
    </div>
    <ScrollToBottomButton visible={!stickToBottom.isSticky} onClick={() => stickToBottom.forceStick()} />
  </div>

  <div class="border-t border-border-subtle px-5 py-3 shrink-0">
    {#if concluded}
      <p class="text-[12px] text-fg-muted">
        This discussion has concluded. Posting is disabled.
      </p>
    {:else}
      <div class="flex gap-2 items-end">
        <textarea
          bind:value={composing}
          onkeydown={handleKeydown}
          disabled={concluded || posting}
          placeholder="Post to the channel (Shift+Enter for newline)"
          aria-label="Channel Message Input"
          rows={1}
          class="flex-1 resize-none rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-2 text-[13px] text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
        ></textarea>
        <Button
          variant="primary"
          size="md"
          onclick={handlePost}
          disabled={!canPost}
          loading={posting}
          class="shrink-0"
        >
          {#snippet children()}{posting ? 'Posting…' : 'Post'}{/snippet}
        </Button>
      </div>
    {/if}
  </div>
</div>
