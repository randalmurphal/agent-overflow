<script lang="ts">
  import { onDestroy, tick, untrack } from 'svelte';
  import type { ChannelMessage } from '../../types/discussion';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { GetChannelMessages, PostChannelMessage } from '../../stores/bindings';
  import { getSettings } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { createUseStickToBottomController } from '../../utils/useStickToBottom.svelte';
  import { relativeTime } from '../../utils/format';
  import Button from '../primitives/Button.svelte';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ScrollToBottomButton from '../chat/ScrollToBottomButton.svelte';
  import { getPathRefsFromMeta } from '../../utils/pathLinkify';

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
  let scrollEl: HTMLDivElement | undefined = $state(undefined);
  let contentEl: HTMLDivElement | undefined = $state(undefined);
  let composerEl: HTMLDivElement | undefined = $state(undefined);
  // No `animationMode` option — Discussion uses the default 'instant'
  // (sync-pin) behavior. Channel messages arrive via the 1s poll in
  // discrete batches, not streamed chunks, so there's no incremental
  // height-growth to chase smoothly. Sync-pin lands the viewport at the
  // new bottom inside the contentRO callback, identical to chat's
  // pre-streaming-restoration UX.
  const stick = createUseStickToBottomController();

  let messages = $derived(pane.channelMessages);
  let status = $derived(pane.channelStatus ?? 'open');
  let concluded = $derived(status === 'concluded' || status === 'closed');
  let canPost = $derived(!concluded && composing.trim().length > 0 && !posting);

  $effect(() => {
    // Only re-run when the channelId prop changes; don't subscribe to the pane
    // state that pollOnce reads/writes, or we loop on every new message.
    if (!channelId) return;
    untrack(() => {
      // Suspend auto-follow until the initial poll lands and we
      // explicitly forceStick. Without this, mergeChannelMessages
      // grows contentEl on the next frame while the controller is
      // still in its default isAtBottom state — the contentRO would
      // sync-pin to the eventual bottom of the seeded batch, but the
      // user perceives the snap because contentEl jumped from empty
      // to populated under their cursor. Setting the escape flag here
      // makes the post-poll forceStick() the single, intentional
      // commitment to the bottom. Mirrors the
      // chat surface's MessageTimeline.$effect.pre escape guard on
      // threadId change.
      stick.setEscapedFromLock(true);
      // Arm the one-shot restore-snap consent for the post-poll
      // `forceStick({reason:'restore'})` below. The defensive
      // `setEscapedFromLock(true)` above clears any prior arm and
      // suspends auto-follow until the initial batch lands; arming
      // restore-snap right after gives the post-poll commit the
      // consent it needs to clear escape and snap to bottom. If the
      // user scrolls between now and the poll completing (rare —
      // typically <100ms), their gesture re-clears the arm via the
      // user-escape paths and the post-poll forceStick NO-OPs.
      stick.armRestoreSnap();
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
    stick.detach();
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
          // Initial batch is in the DOM; snap to bottom. Subsequent
          // async Streamdown row growth fires contentRO positive deltas
          // which sync-pin scrollTop to the new bottom each frame —
          // the view stays at the bottom without any layered animation
          // on top of the content arrival.
          await tick();
          if (generation === pollGeneration) {
            // reason:'restore' so an intervening user scroll-up (which
            // re-clears the restore-snap consent armed in the channel
            // setup $effect above) preserves the user's position
            // instead of slamming them to the bottom of the initial
            // batch.
            stick.forceStick({ reason: 'restore' });
          }
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

  // Publish the controller on the pane so external surfaces (sidebar
  // resizers, resizable drawers) can acquire a `pauseAutoScroll()` lease
  // during gestures. The effect's return function detaches symmetrically
  // when the pane reference changes and on component teardown, so a
  // stale pointer to a torn-down controller can't leak.
  $effect(() => {
    pane.attachScrollController(stick);
    return () => pane.detachScrollController(stick);
  });

  // Bind the controller to the actual DOM elements. The content RO and
  // wheel/scroll/keydown/touch listeners all start here. Re-runs if
  // either ref changes (thread switch / HMR).
  $effect(() => {
    if (!scrollEl || !contentEl) return;
    stick.attach(scrollEl, contentEl);
  });

  // Composer-section RO. Discussion's textarea + button live in a
  // sibling flex section that's NOT inside the controller's contentEl,
  // so a height change there (e.g. the concluded-toggle swapping the
  // textarea+button for a "Discussion has concluded" paragraph)
  // shrinks/grows the scrollEl's clientHeight without firing the
  // content RO. notifyContentMaybeGrew re-pins scrollTop to the new
  // target so a sticky user doesn't drift away from the last message.
  // The textarea itself is `rows={1}` with no autosize, so the more
  // dramatic Shift+Enter case doesn't actually change height — but the
  // RO costs nothing per-event and future-proofs against a textarea
  // that grows.
  $effect(() => {
    if (!composerEl) return;
    const observed = composerEl;
    const ro = new ResizeObserver(() => stick.notifyContentMaybeGrew());
    ro.observe(observed);
    return () => ro.disconnect();
  });

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
    <span class="inline-flex items-center gap-1.5 rounded-[var(--radius-field)] border px-2 py-0.5 text-[0.625rem] font-medium uppercase tracking-wide
      {concluded ? 'border-border-subtle bg-surface-2/40 text-fg-muted' : 'border-success/30 bg-success/10 text-success'}">
      <span class="w-1.5 h-1.5 rounded-full {concluded ? 'bg-fg-subtle' : 'bg-success'}" aria-hidden="true"></span>
      {statusLabel()}
    </span>
    <span class="text-[0.6875rem] text-fg-muted tabular-nums">
      {messages.length} {messages.length === 1 ? 'message' : 'messages'}
    </span>
    {#if pollError}
      <span role="alert" class="ml-auto text-[0.6875rem] text-error truncate max-w-[280px]" title={pollError}>
        Poll error: {pollError}
      </span>
    {/if}
  </div>

  <div class="relative flex-1 min-h-0 flex flex-col">
    <!-- overflow-anchor: none disables the browser's scroll-anchor
         heuristic, which would adjust scrollTop when content above the
         viewport changes size to keep the topmost-visible element fixed.
         That fights the controller's contentRO sync-pin: Streamdown's
         async typesetting (shiki / KaTeX / mermaid) growing rows above
         the viewport on a sticky session would produce visible scrollTop
         oscillation between the browser's anchor adjustment and our
         re-pin. See frontend/AGENTS.md § Scroll architecture. -->
    <div
      bind:this={scrollEl}
      class="flex-1 min-h-0 overflow-y-auto px-5 py-4"
      style:overflow-anchor="none"
      role="log"
      aria-live="polite"
      aria-label="Discussion Channel Messages"
      data-testid="channel-message-list"
    >
      <div bind:this={contentEl} class="space-y-3">
        {#if loadingInitial}
          <div class="text-[0.75rem] text-fg-subtle">Loading channel messages…</div>
        {:else if messages.length === 0}
          <div class="text-[0.75rem] text-fg-subtle">
            No messages yet. Participants will begin speaking as their turns complete.
          </div>
        {:else}
          {#each messages as msg (msg.id || msg.sequence)}
            <div class="rounded-[var(--radius-card)] border {messageAccentClass(msg)} px-3.5 py-2.5">
              <div class="flex items-center gap-2 mb-1.5">
                <span class="rounded-[var(--radius-field)] border px-1.5 py-0.5 text-[0.625rem] font-semibold uppercase tracking-[0.14em] {roleBadgeClass(msg)}">
                  {roleLabel(msg)}
                </span>
                <span class="text-[0.6875rem] text-fg-hint tabular-nums">#{msg.sequence}</span>
                <span class="ml-auto text-[0.6875rem] text-fg-hint">
                  {relativeTime(msg.createdAt, getSettings().timestampFormat)}
                </span>
              </div>
              <ChatMarkdown
                source={msg.content}
                workspacePath={paneWorkspacePath(pane)}
                pathRefs={getPathRefsFromMeta(msg.meta) ?? []}
                class="text-[0.8125rem] text-fg break-words"
              />
            </div>
          {/each}
        {/if}
      </div>
    </div>
    <ScrollToBottomButton visible={!stick.isAtBottom} onClick={() => stick.forceStick()} />
  </div>

  <div
    bind:this={composerEl}
    class="border-t border-border-subtle px-5 py-3 shrink-0"
    data-testid="channel-composer-section"
  >
    {#if concluded}
      <p class="text-[0.75rem] text-fg-muted">
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
          class="flex-1 resize-none rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 px-3 py-2 text-[0.8125rem] text-fg placeholder:text-fg-hint focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
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
