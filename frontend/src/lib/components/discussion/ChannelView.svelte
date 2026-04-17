<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import type { ChannelMessage } from '../../types/discussion';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { GetChannelMessages, PostChannelMessage } from '../../stores/bindings';
  import { getSettings } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { relativeTime } from '../../utils/format';
  import Markdown from '../shared/Markdown.svelte';

  let {
    pane,
    channelId,
  }: {
    pane: ThreadPane;
    channelId: string;
  } = $props();

  const POLL_INTERVAL_MS = 2500;
  const PAGE_LIMIT = 200;

  let pollGeneration = 0;
  let pollTimer: ReturnType<typeof setTimeout> | null = null;
  let lastChannelId = '';
  let composing = $state('');
  let posting = $state(false);
  let pollError = $state<string | null>(null);
  let loadingInitial = $state(true);
  let scrollContainer: HTMLDivElement | undefined = $state(undefined);
  let userNearBottom = $state(true);
  const NEAR_BOTTOM_THRESHOLD = 80;

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
      pollError = String(err);
    } finally {
      if (generation === pollGeneration) {
        if (initial) {
          loadingInitial = false;
        }
        pollTimer = setTimeout(() => pollOnce(generation, false), POLL_INTERVAL_MS);
      }
    }
  }

  $effect(() => {
    // Auto-scroll on new messages when user hasn't scrolled away. Only track
    // `messages.length` here — reading `userNearBottom` tracked would cause
    // the scroll-triggered handler to re-invalidate this effect in a loop.
    const len = messages.length;
    if (len === 0) return;
    untrack(() => {
      if (scrollContainer && userNearBottom) {
        requestAnimationFrame(() => {
          if (scrollContainer) {
            scrollContainer.scrollTop = scrollContainer.scrollHeight;
          }
        });
      }
    });
  });

  function handleScroll(): void {
    if (!scrollContainer) return;
    const { scrollTop, scrollHeight, clientHeight } = scrollContainer;
    userNearBottom = scrollHeight - scrollTop - clientHeight <= NEAR_BOTTOM_THRESHOLD;
  }

  async function handlePost(): Promise<void> {
    const content = composing.trim();
    if (!content || posting || concluded) return;
    posting = true;
    const savedText = composing;
    composing = '';
    try {
      await PostChannelMessage(channelId, content);
      // Immediate poll to surface our message rather than wait for interval.
      void pollOnce(pollGeneration, false);
    } catch (err) {
      console.error('Failed to post channel message:', err);
      composing = savedText;
      addToast('error', `Failed to post message: ${err}`);
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
      return 'border-accent/30 bg-accent/5';
    }
    return 'border-border/60 bg-surface-1/55';
  }

  function roleLabel(msg: ChannelMessage): string {
    if (msg.fromType === 'human') return 'You';
    return msg.fromRole?.trim() || 'agent';
  }

  function roleBadgeClass(msg: ChannelMessage): string {
    if (msg.fromType === 'human') {
      return 'bg-accent/15 text-accent border-accent/25';
    }
    return 'bg-surface-2/60 text-text-secondary border-border/60';
  }

  function statusLabel(): string {
    if (concluded) return 'Concluded';
    return 'Live';
  }
</script>

<div class="flex h-full flex-col min-h-0">
  <div class="border-b border-border/60 bg-surface-1/70 px-4 py-2.5 flex items-center gap-3 shrink-0">
    <span class="inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] font-medium
      {concluded ? 'border-text-secondary/30 bg-surface-2/50 text-text-secondary' : 'border-success/30 bg-success/10 text-success'}">
      <span class="w-1.5 h-1.5 rounded-full {concluded ? 'bg-text-secondary' : 'bg-success'}" aria-hidden="true"></span>
      {statusLabel()}
    </span>
    <span class="text-xs text-text-secondary">
      {messages.length} {messages.length === 1 ? 'message' : 'messages'}
    </span>
    {#if pollError}
      <span role="alert" class="ml-auto text-[11px] text-error truncate max-w-[280px]" title={pollError}>
        Poll error: {pollError}
      </span>
    {/if}
  </div>

  <div
    bind:this={scrollContainer}
    onscroll={handleScroll}
    class="flex-1 min-h-0 overflow-y-auto px-4 py-3 space-y-3"
    role="log"
    aria-live="polite"
    aria-label="Discussion channel messages"
  >
    {#if loadingInitial}
      <div class="text-xs text-text-secondary/70">Loading channel messages...</div>
    {:else if messages.length === 0}
      <div class="text-xs text-text-secondary/70">
        No messages yet. Participants will begin speaking as their turns complete.
      </div>
    {:else}
      {#each messages as msg (msg.id || msg.sequence)}
        <div class="rounded-2xl border {messageAccentClass(msg)} px-3 py-2.5">
          <div class="flex items-center gap-2 mb-1.5">
            <span class="rounded-md border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-[0.14em] {roleBadgeClass(msg)}">
              {roleLabel(msg)}
            </span>
            <span class="text-[11px] text-text-secondary/70">#{msg.sequence}</span>
            <span class="ml-auto text-[11px] text-text-secondary/60">
              {relativeTime(msg.createdAt, getSettings().timestampFormat)}
            </span>
          </div>
          <div class="text-sm text-text-primary break-words">
            <Markdown content={msg.content} />
          </div>
        </div>
      {/each}
    {/if}
  </div>

  <div class="border-t border-border/60 bg-surface-1/70 px-4 py-3 shrink-0">
    {#if concluded}
      <p class="text-xs text-text-secondary/80">
        This discussion has concluded. Posting is disabled.
      </p>
    {:else}
      <div class="flex gap-2 items-end">
        <textarea
          bind:value={composing}
          onkeydown={handleKeydown}
          disabled={concluded || posting}
          placeholder="Post to the channel (Shift+Enter for newline)"
          aria-label="Channel message input"
          rows={1}
          class="flex-1 resize-none rounded-lg border border-border bg-surface-0 px-3 py-2.5 text-sm text-text-primary placeholder:text-text-secondary/50 focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
        ></textarea>
        <button
          type="button"
          onclick={handlePost}
          disabled={!canPost}
          class="shrink-0 rounded-lg bg-accent px-4 py-2.5 text-sm font-medium text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {posting ? 'Posting...' : 'Post'}
        </button>
      </div>
    {/if}
  </div>
</div>
