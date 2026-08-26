<script lang="ts">
  import { onDestroy, tick, untrack } from 'svelte';
  import type { ChannelMessage, ChannelStatePayload } from '../../types/discussion';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { PostChannelMessage, ConcludeDiscussion } from '../../stores/bindings';
  import { refreshDiscussionChannel, DISCUSSION_CHANNEL_FETCH_LIMIT } from '../../stores/eventsDiscussion';
  import { getSettings } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { createUseStickToBottomController } from '../../utils/scroll/index.svelte';
  import { isLiveContentActive, LIVE_CONTENT_ACTIVE_HOLD_MS } from '../../utils/liveContentActivity';
  import { relativeTime } from '../../utils/format';
  import Button from '../primitives/Button.svelte';
  import OverlayScrollbar from '../shared/OverlayScrollbar.svelte';
  import ChannelHeader from './ChannelHeader.svelte';
  import ChannelMessageCard from './ChannelMessageCard.svelte';
  import ScrollToBottomButton from '../chat/ScrollToBottomButton.svelte';
  import { EMPTY_PATH_REFS, getPathRefsFromMeta } from '../../utils/pathLinkify';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  let {
    pane,
    channelId,
  }: {
    pane: ThreadPane;
    channelId: string;
  } = $props();

  let lastChannelId = '';
  let loadGeneration = 0;
  let composing = $state('');
  let posting = $state(false);
  let concluding = $state(false);
  let loadError = $state<string | null>(null);
  let loadingInitial = $state(true);
  let scrollEl: HTMLDivElement | undefined = $state(undefined);
  let contentEl: HTMLDivElement | undefined = $state(undefined);
  let composerEl: HTMLDivElement | undefined = $state(undefined);
  // Discussion now streams the current speaker's in-flight text just like
  // chat: `pane.channelLastLiveContentAt` is stamped by the channel-state
  // layer on genuinely-new messages AND on live-tail growth (see
  // threadChannelState.svelte.ts). Keyed off the channel's own stamp
  // rather than the pane's timeline stamp (a discussion pane's
  // `lastLiveContentAt` tracks the timeline, which this surface doesn't
  // render). Liveness only — growth physics is decided by the
  // controller, identically to the chat surface.
  function channelLiveContentActive(): boolean {
    return isLiveContentActive(
      performance.now(),
      pane.channelLastLiveContentAt,
      LIVE_CONTENT_ACTIVE_HOLD_MS,
    );
  }

  const stick = createUseStickToBottomController({
    liveContentActive: channelLiveContentActive,
  });

  let messages = $derived(pane.channelMessages);
  let status = $derived(pane.channelStatus);
  let concluded = $derived(status === 'concluded' || status === 'closed');
  let canPost = $derived(!concluded && composing.trim().length > 0 && !posting);
  let liveTail = $derived(pane.channelLiveTail);
  let awaitingResponse = $derived(pane.channelAwaitingResponse);
  let currentSpeakerRole = $derived(pane.channelCurrentSpeakerRole ?? 'agent');
  let turnCount = $derived(pane.channelTurnCount);
  let maxTurns = $derived(pane.channelMaxTurns);
  let participants = $derived(pane.channelParticipants);
  // "No tail text yet" covers both no live tail at all and a live tail
  // whose upsert/delta hasn't carried any text yet (a freshly-created
  // assistant_text item before its first chunk).
  let showLiveTail = $derived(status === 'open' && !!liveTail && liveTail.text.length > 0);
  let showSpeakingIndicator = $derived(status === 'open' && awaitingResponse && !showLiveTail);

  $effect(() => {
    // Only re-run when the channelId prop changes; don't subscribe to the
    // pane state the load path reads/writes, or we loop on every push.
    if (!channelId) return;
    const generation = ++loadGeneration;
    untrack(() => {
      // Suspend auto-follow until the initial load lands and we
      // explicitly forceStick: armRestoreSnap sets the defensive escape
      // (without it, the initial batch growing contentEl on the next
      // frame while the controller is still in its default isAtBottom
      // state would sync-pin to the eventual bottom mid-flight) and arms
      // the one-shot restore-snap consent for the post-load
      // forceStick({reason:'restore'}) below. Simplified mirror of the
      // switch-edge choreography MessageTimeline runs through
      // components/chat/timelineRestore.svelte.ts (handleSwitchEdgePre →
      // restore effect) — no snapshots here, just the consent arming.
      stick.armRestoreSnap();
      // Only wipe the channel buffer when switching to a different
      // channel — re-entry of the same channel keeps whatever the last
      // snapshot said (status is authoritative now, not seeded).
      if (lastChannelId !== channelId) {
        pane.clearChannel();
        lastChannelId = channelId;
      }
      void loadInitial(generation);
    });

    return () => {
      // Bump generation to cancel an in-flight load for this channel.
      loadGeneration++;
    };
  });

  onDestroy(() => {
    loadGeneration++;
    stick.detach();
  });

  async function loadInitial(generation: number): Promise<void> {
    loadingInitial = true;
    loadError = null;
    try {
      const fetched = await refreshDiscussionChannel(pane);
      if (generation !== loadGeneration) return;
      if (fetched.length === DISCUSSION_CHANNEL_FETCH_LIMIT) {
        console.warn(
          `Channel ${channelId}: initial load returned ${DISCUSSION_CHANNEL_FETCH_LIMIT} messages — history may be truncated.`,
        );
      }
    } catch (err) {
      if (generation !== loadGeneration) return;
      console.error('Failed to load channel:', err);
      loadError = errString(err);
    } finally {
      if (generation === loadGeneration) {
        loadingInitial = false;
        // Initial batch is in the DOM; snap to bottom. Subsequent live
        // pushes (discussion:message, live-tail growth) route through
        // the same contentRO the chat surface uses and follow the bottom
        // with the same glide.
        await tick();
        if (generation === loadGeneration) {
          // reason:'restore' so an intervening user scroll-up (which
          // re-clears the restore-snap consent armed above) preserves
          // the user's position instead of slamming them to the bottom.
          stick.forceStick({ reason: 'restore' });
        }
      }
    }
  }

  function retryInitialLoad(): void {
    // Bump the generation exactly like the mount effect: reusing the
    // current one would let two rapid retries run two un-cancellable
    // concurrent loads with the slower resolver winning.
    const generation = ++loadGeneration;
    void loadInitial(generation);
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
  // content RO. The composer-geometry observation re-pins scrollTop to
  // the new target so a sticky user doesn't drift away from the last
  // message. The textarea itself is `rows={1}` with no autosize, so the
  // more dramatic Shift+Enter case doesn't actually change height — but
  // the RO costs nothing per-event and future-proofs against a textarea
  // that grows.
  $effect(() => {
    if (!composerEl) return;
    const observed = composerEl;
    const ro = new ResizeObserver(() => stick.observe('composer-geometry'));
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
      const created = await PostChannelMessage(channelId, content);
      // Apply our own post immediately rather than waiting on the
      // discussion:message echo; applyChannelMessage's sequence dedupe
      // makes the later push a no-op.
      pane.applyChannelMessage(created as ChannelMessage);
    } catch (err) {
      console.error('Failed to post channel message:', err);
      composing = savedText;
      addToast('error', `Failed to post message: ${errString(err)}`);
    } finally {
      posting = false;
    }
  }

  // Moderator "conclude now" affordance (ChannelHeader's Conclude
  // button). `concluding` guards double-clicks the same way `posting`
  // guards a double-submit of the composer. On success the returned
  // ChannelStatePayload is applied through the same pane surface
  // discussion:state pushes use, so the header flips to Concluded
  // without waiting on the push echo — mirrors how handlePost applies
  // PostChannelMessage's own returned message immediately. Failure
  // surfaces the same way handlePost's does: a console.error plus an
  // error toast, no local error banner.
  async function handleConclude(): Promise<void> {
    if (concluding || concluded) return;
    concluding = true;
    try {
      const payload = await ConcludeDiscussion(channelId);
      pane.applyChannelState(payload as ChannelStatePayload);
    } catch (err) {
      console.error('Failed to conclude discussion:', err);
      addToast('error', `Failed to conclude discussion: ${errString(err)}`);
    } finally {
      concluding = false;
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    // Enter mid-IME-composition confirms the candidate — the composed text
    // is not in the textarea's value yet, so posting would truncate it.
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handlePost();
    }
  }

  function messageAccentClass(msg: ChannelMessage): string {
    if (msg.fromType === 'human') return 'border-accent/25 bg-accent/5';
    if (msg.fromType === 'system') return 'border-border-subtle bg-surface-2/20';
    return 'border-border-subtle bg-card/30';
  }

  function roleLabel(msg: ChannelMessage): string {
    if (msg.fromType === 'human') return 'You';
    if (msg.fromType === 'system') return 'Moderator';
    return msg.fromRole?.trim() || 'agent';
  }

  function roleBadgeClass(msg: ChannelMessage): string {
    if (msg.fromType === 'human') return 'bg-accent/10 text-accent border-accent/25';
    if (msg.fromType === 'system') return 'bg-fg-subtle/10 text-fg-subtle border-border-subtle';
    return 'bg-surface-2/40 text-fg-muted border-border-subtle';
  }

  function statusLabel(): string {
    if (status === null) return 'Loading';
    return concluded ? 'Concluded' : 'Live';
  }
</script>

<div class="flex h-full flex-col min-h-0">
  <ChannelHeader
    {concluded}
    statusLabel={statusLabel()}
    messageCount={messages.length}
    {status}
    {turnCount}
    {maxTurns}
    {awaitingResponse}
    {currentSpeakerRole}
    {participants}
    {loadError}
    onConclude={handleConclude}
    {concluding}
  />

  <div class="relative flex-1 min-h-0 flex flex-col">
    <!-- overflow-anchor: none disables the browser's scroll-anchor
         heuristic, which would adjust scrollTop when content above the
         viewport changes size to keep the topmost-visible element fixed.
         That fights the controller's contentRO sync-pin/spring. See
         docs/architecture/frontend-scroll.md. -->
    <div
      bind:this={scrollEl}
      class="pane-scroll-surface flex-1 min-h-0 overflow-y-auto px-5 py-4"
      style:overflow-anchor="none"
      role="log"
      aria-live="polite"
      aria-label="Discussion Channel Messages"
      data-testid="channel-message-list"
    >
      <div bind:this={contentEl} class="space-y-3">
        {#if loadingInitial}
          <div class="text-[0.75rem] text-fg-subtle">Loading channel messages…</div>
        {:else if loadError}
          <div
            class="rounded-[var(--radius-card)] border border-error/30 bg-error/5 px-3.5 py-2.5 text-[0.75rem] text-error flex items-center gap-3"
          >
            <span class="flex-1">Failed to load channel: {loadError}</span>
            <Button variant="secondary" size="xs" onclick={retryInitialLoad}>
              {#snippet children()}Retry{/snippet}
            </Button>
          </div>
        {:else}
          {#if messages.length === 0 && !showLiveTail && !showSpeakingIndicator}
            <div class="text-[0.75rem] text-fg-subtle">
              No messages yet. Post a message to start the discussion.
            </div>
          {/if}
          {#each messages as msg (msg.id || msg.sequence)}
            <ChannelMessageCard
              workspacePath={paneWorkspacePath(pane)}
              accentClass={messageAccentClass(msg)}
              badgeClass={roleBadgeClass(msg)}
              roleLabel={roleLabel(msg)}
              sequenceLabel={`#${msg.sequence}`}
              timestampLabel={relativeTime(msg.createdAt, getSettings().timestampFormat)}
              content={msg.content}
              pathRefs={getPathRefsFromMeta(msg.meta) ?? EMPTY_PATH_REFS}
            />
          {/each}
          {#if showLiveTail && liveTail}
            <ChannelMessageCard
              workspacePath={paneWorkspacePath(pane)}
              accentClass="border-border-subtle bg-card/30"
              badgeClass="bg-surface-2/40 text-fg-muted border-border-subtle"
              roleLabel={currentSpeakerRole}
              content={liveTail.text}
              streaming={true}
            />
          {:else if showSpeakingIndicator}
            <div class="text-[0.75rem] text-fg-subtle italic px-1" data-testid="channel-speaking-indicator">
              {currentSpeakerRole} is preparing a response…
            </div>
          {/if}
        {/if}
      </div>
    </div>
    <!-- This surface's scrollbar: a zero-width sibling overlay, same
         contract and intent wiring as MessageTimeline's (which carries
         the full rationale, including the composited-scrolling hint the
         shared pane-scroll-surface class applies). -->
    <OverlayScrollbar
      target={scrollEl}
      content={contentEl}
      ariaLabel="Scroll discussion messages"
      placement="inset-y-0 right-0.5 w-1.5"
      ownerDrivenPosition={() => stick.isSticky}
      onUserScrollStart={() => stick.setEscapedFromLock(true)}
      onUserScrollEnd={(atBottom) => {
        if (atBottom) stick.forceStick();
      }}
    />
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
