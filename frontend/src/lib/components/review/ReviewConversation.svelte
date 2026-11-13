<script lang="ts">
  import { tick, untrack } from 'svelte';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewConversationCommits from './ReviewConversationCommits.svelte';
  import ReviewConversationThread from './ReviewConversationThread.svelte';
  import ReviewHeldReplies from './ReviewHeldReplies.svelte';
  import ReviewPRCommentField from './ReviewPRCommentField.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { relativeTime } from '../../utils/format';
  import { REVIEW_JUMP_PULSE_MS } from '../../utils/reviewJumpPulse';
  import { visibleBody } from '../../utils/reviewComments';
  import { authorDisplayName } from '../../utils/reviewIdentity';
  import { READINESS_PILL_CLASS, reviewEventLabel, reviewEventTone } from '../../utils/reviewReadiness';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';

  // The Conversation section's body: ONE chronological feed (newest
  // first) interleaving thread cards, review verdicts, forge events
  // (approvals, merges, draft flips, auto-merge) and commit pushes, the
  // forge's overview timeline made compact. Entries hang off one
  // vertical rail so the sequence reads at a glance; the avatar sits on
  // the rail and the card beside it. The store owns the feed: an entry
  // keeps its place and arrivals join at theirs. This component holds the
  // reader's place: while the section is scrolled or focus is inside it,
  // the entry at the top of the viewport stays put to the pixel across
  // arrivals and growth above it (WebKit has no `overflow-anchor`, so the
  // scroll is compensated here, for every engine alike). Under the feed:
  // the count Hide resolved hides, held replies whose thread is gone
  // (kept, to delete or keep out of the review) and the one "Comment…"
  // field.

  interface Props {
    review: ReviewPaneState;
  }

  let { review }: Props = $props();

  let rootEl: HTMLElement | undefined = $state();
  let listEl: HTMLElement | undefined = $state();

  // The section's scroll body (ReviewResizableBody) wraps this feed.
  function scrollerOf(): HTMLElement | null {
    return rootEl?.parentElement ?? null;
  }

  function entryOf(id: string): HTMLElement | null {
    return listEl?.querySelector<HTMLElement>(`[data-feed-id="${CSS.escape(id)}"]`) ?? null;
  }

  // The reader's place is held while the section is scrolled or focus is
  // inside it; at the top, arrivals simply push the cards down.
  function placeHeld(scroller: HTMLElement): boolean {
    return scroller.scrollTop > 0 || (!!rootEl && rootEl.contains(document.activeElement));
  }

  // The first entry whose bottom is below the viewport's top: entries are
  // stacked in order, so a binary search finds it in a long feed.
  function topEntry(scroller: HTMLElement): { id: string; offset: number } | null {
    const items = listEl?.children;
    if (!items || items.length === 0) return null;
    const top = scroller.getBoundingClientRect().top;
    let lo = 0;
    let hi = items.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (items[mid].getBoundingClientRect().bottom > top) hi = mid;
      else lo = mid + 1;
    }
    const el = items[lo] as HTMLElement;
    const id = el.dataset.feedId;
    return id ? { id, offset: el.getBoundingClientRect().top - top } : null;
  }

  // Until the remembered place is restored, the mount's own layout must
  // not overwrite it.
  let placed = false;

  function recordPlace(scroller: HTMLElement): void {
    review.setConversationAnchor(topEntry(scroller));
    // The chip goes once its newest arrival has been in view.
    const newest = review.conversationNewestArrivalId;
    const el = newest ? entryOf(newest) : null;
    if (!el) return;
    const view = scroller.getBoundingClientRect();
    const rect = el.getBoundingClientRect();
    if (rect.bottom > view.top && rect.top < view.bottom) review.acknowledgeConversationArrivals();
  }

  // Keep the anchored entry where it was on screen, then record where
  // things stand. Runs after every feed change and every size change.
  function holdPlace(): void {
    const scroller = scrollerOf();
    if (!scroller || !placed) return;
    const anchor = review.conversationAnchor;
    const el = anchor ? entryOf(anchor.id) : null;
    if (anchor && el && placeHeld(scroller)) {
      const drift = el.getBoundingClientRect().top - scroller.getBoundingClientRect().top - anchor.offset;
      if (Math.abs(drift) >= 0.5) scroller.scrollTop += drift;
    }
    recordPlace(scroller);
  }

  // Scrolls the section alone (never the diff around it) so `el` is in
  // view, the least distance.
  function scrollEntryIntoView(scroller: HTMLElement, el: HTMLElement): void {
    const view = scroller.getBoundingClientRect();
    const rect = el.getBoundingClientRect();
    if (rect.top < view.top) scroller.scrollTop += rect.top - view.top;
    else if (rect.bottom > view.bottom) scroller.scrollTop += Math.min(rect.bottom - view.bottom, rect.top - view.top);
  }

  // Restore the reading place once the cards have laid out (a markdown
  // body reports its height a flush after mount): by entry, else by the
  // remembered offset. A staged jump decides the place instead.
  $effect(() => {
    const scroller = scrollerOf();
    if (!scroller) return;
    let live = true;
    void tick().then(() => {
      if (!live || !scroller.isConnected) return;
      if (!review.pendingConversationThreadId && !review.pendingConversationArrivalId) {
        const anchor = review.conversationAnchor;
        const el = anchor ? entryOf(anchor.id) : null;
        if (anchor && el) {
          scroller.scrollTop += el.getBoundingClientRect().top - scroller.getBoundingClientRect().top - anchor.offset;
        } else {
          const offset = review.overviewSectionScrollTop('conversation');
          if (offset > 0) scroller.scrollTop = offset;
        }
      }
      placed = true;
      holdPlace();
    });
    const onScroll = () => { if (placed) recordPlace(scroller); };
    scroller.addEventListener('scroll', onScroll, { passive: true });
    return () => {
      live = false;
      placed = false;
      scroller.removeEventListener('scroll', onScroll);
    };
  });

  $effect(() => {
    void review.conversationFeed;
    untrack(holdPlace);
  });

  $effect(() => {
    const list = listEl;
    if (!list) return;
    const observer = new ResizeObserver(() => holdPlace());
    observer.observe(list);
    return () => observer.disconnect();
  });

  $effect(() => review.attachConversationFocusProbe(() => !!rootEl && rootEl.contains(document.activeElement)));

  // The chip's press, or the reader's own posted comment: bring the entry
  // into view.
  $effect(() => {
    const target = review.pendingConversationArrivalId;
    if (!target) return;
    untrack(() => {
      review.consumePendingConversationArrival();
      void tick().then(() => {
        const scroller = scrollerOf();
        const el = entryOf(target);
        if (scroller && el) scrollEntryIntoView(scroller, el);
      });
    });
  });

  const diffPaths = $derived(new Set(review.files.map((file) => file.path)));

  function verdictTime(submittedAt: string): string {
    const ms = Date.parse(submittedAt);
    return Number.isNaN(ms) ? '' : relativeTime(ms);
  }

  // An event's ref is a commit (a hex oid, shown abbreviated) or a name
  // (a branch, a requested reviewer's login), shown whole.
  function abbreviateEventRef(ref: string): string {
    return /^[0-9a-f]{12,}$/i.test(ref) ? ref.slice(0, 7) : ref;
  }

  function verdictKind(state: string): 'approved' | 'changes' | 'dismissed' | 'comment' {
    switch (state.toUpperCase()) {
      case 'APPROVED':
        return 'approved';
      case 'CHANGES_REQUESTED':
        return 'changes';
      case 'DISMISSED':
        return 'dismissed';
      default:
        return 'comment';
    }
  }

  // A dismissed review (GitHub: an approval withdrawn) keeps its card,
  // since its body still stands, and says what became of it.
  const VERDICT_LABEL = { approved: 'approved', changes: 'requested changes', dismissed: 'review dismissed', comment: 'reviewed' } as const;
  const VERDICT_CHIP = {
    approved: 'bg-success/12 text-success',
    changes: 'bg-error/12 text-error',
    dismissed: 'bg-warning/12 text-warning',
    comment: 'bg-surface-2 text-fg-muted',
  } as const;
  const VERDICT_CARD = {
    approved: 'border-success/45',
    changes: 'border-error/45',
    dismissed: 'border-border',
    comment: 'border-border',
  } as const;
  const VERDICT_HEAD = {
    approved: 'bg-success/8',
    changes: 'bg-error/8',
    dismissed: 'bg-surface-2/60',
    comment: 'bg-surface-2/60',
  } as const;

  // One-shot scroll to a jumped-to thread (inline strip, stepper or peek
  // control). Runs after the cards render; consuming clears it so a later
  // remount of the row does not replay the scroll. The landed-on card
  // pulses: it is held here, with a generation so the same card pulses
  // again on a repeat jump, for the pulse's lifetime and then forgotten,
  // so a later remount of the card does not replay the pulse either.
  let pulse: { threadId: string; generation: number } | null = $state(null);
  let pulseTimer: ReturnType<typeof setTimeout> | undefined;
  $effect(() => {
    const target = review.pendingConversationThreadId;
    if (!target || !rootEl) return;
    const el = rootEl.querySelector(`[data-thread-id="${CSS.escape(target)}"]`);
    review.consumePendingConversationThreadId();
    if (!el) return;
    // A freshly mounted feed's card bodies lay out a flush after mount
    // (ReviewResizableBody waits the same tick), so scrolling now would
    // aim at a shorter feed. The jump owns the section's reading offset
    // (the store drops the remembered one when it stages the jump):
    // record where it landed, scroll event or not, so a remount of the
    // overview row comes back here.
    const scroller = rootEl.parentElement;
    void tick().then(() => {
      if (!el.isConnected) return;
      el.scrollIntoView({ block: 'nearest' });
      if (scroller) review.setOverviewSectionScrollTop('conversation', scroller.scrollTop);
    });
    untrack(() => {
      pulse = { threadId: target, generation: (pulse?.generation ?? 0) + 1 };
      clearTimeout(pulseTimer);
      pulseTimer = setTimeout(() => { pulse = null; }, REVIEW_JUMP_PULSE_MS);
    });
  });
  $effect(() => () => clearTimeout(pulseTimer));
</script>

<div bind:this={rootEl} class="relative px-3 py-3.5 text-xs" data-testid="review-conversation">
  {#if review.conversationFeed.length > 1}
    <!-- The rail: avatar discs are 28px wide from x=12, so their center
         is x=26; the line runs from the first disc to the last. -->
    <div class="pointer-events-none absolute bottom-7 left-[25.5px] top-7 w-px bg-border" aria-hidden="true"></div>
  {/if}
  <div bind:this={listEl} class="flex flex-col gap-4">
    {#each review.conversationFeed as entry (entry.id)}
      <div class="min-w-0" data-feed-id={entry.id}>
      {#if entry.kind === 'thread'}
        <ReviewConversationThread
          {review}
          thread={entry.thread}
          inDiff={entry.thread.path !== '' && diffPaths.has(entry.thread.path)}
          pulse={pulse?.threadId === entry.thread.id ? pulse.generation : 0}
        />
      {:else if entry.kind === 'verdict'}
        {@const body = visibleBody(entry.verdict.body)}
        {@const kind = verdictKind(entry.verdict.state)}
        <article
          class="grid grid-cols-[28px_minmax(0,1fr)] gap-x-3"
          data-testid="review-conversation-verdict"
        >
          <div class="relative z-10 rounded-full ring-[3px] ring-surface-0/70">
            <ReviewAvatar login={entry.verdict.authorLogin} name={entry.verdict.authorName} size={28} />
          </div>
          <div class="min-w-0 overflow-hidden rounded-[var(--radius-control)] border bg-surface-1 {VERDICT_CARD[kind]}">
            <div class="flex min-w-0 items-center gap-1.5 px-3 py-2 {VERDICT_HEAD[kind]} {body !== '' ? 'border-b border-border-subtle' : ''}">
              <span class="truncate font-semibold text-fg">{authorDisplayName(entry.verdict)}</span>
              <span class="shrink-0 text-fg-muted">{VERDICT_LABEL[kind]}</span>
              {#if verdictTime(entry.verdict.submittedAt)}
                <span class="shrink-0 text-fg-subtle">· {verdictTime(entry.verdict.submittedAt)}</span>
              {/if}
              <span class="min-w-0 flex-1"></span>
              <span class="shrink-0 rounded-full px-2 py-px text-[0.6875rem] {VERDICT_CHIP[kind]}">{VERDICT_LABEL[kind] === 'reviewed' ? 'commented' : VERDICT_LABEL[kind]}</span>
            </div>
            {#if body !== ''}
              <div class="px-3.5 py-3">
                <ChatMarkdown source={body} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
              </div>
            {/if}
          </div>
        </article>
      {:else if entry.kind === 'event'}
        <!-- A forge event is one line on the rail, like a push: who did
             what, when, and the forge's own words under it. -->
        <div class="grid grid-cols-[28px_minmax(0,1fr)] gap-x-3" data-testid="review-conversation-event" data-kind={entry.event.kind}>
          <div class="relative z-10 rounded-full ring-[3px] ring-surface-0/70">
            <ReviewAvatar login={entry.event.actorLogin} name={entry.event.actorName} size={28} />
          </div>
          <div class="flex min-h-7 min-w-0 flex-wrap items-center gap-x-1.5 gap-y-0.5 text-[0.75rem]">
            <span class="truncate font-semibold text-fg">{authorDisplayName({ authorLogin: entry.event.actorLogin, authorName: entry.event.actorName })}</span>
            <span class="shrink-0 rounded-full px-1.5 py-px text-[0.6875rem] {READINESS_PILL_CLASS[reviewEventTone(entry.event.kind)]}">{reviewEventLabel(entry.event.kind)}</span>
            {#if entry.event.ref}
              <!-- A name can be a branch or a list of reviewers of any
                   length: it wraps inside the row. -->
              <span class="min-w-0 break-words font-mono text-[0.6875rem] text-fg-muted" data-testid="review-conversation-event-ref">{abbreviateEventRef(entry.event.ref)}</span>
            {/if}
            {#if verdictTime(entry.event.at)}
              <span class="shrink-0 text-fg-subtle">· {verdictTime(entry.event.at)}</span>
            {/if}
            {#if entry.event.text && entry.event.text !== reviewEventLabel(entry.event.kind)}
              <span class="basis-full truncate text-fg-muted" title={entry.event.text}>{entry.event.text}</span>
            {/if}
          </div>
        </div>
      {:else}
        <ReviewConversationCommits {review} author={entry.author} commits={entry.commits} />
      {/if}
      </div>
    {/each}
  </div>

  {#if review.conversationFeed.length === 0 && review.hiddenResolvedCount === 0}
    <div class="px-1 py-1 text-fg-muted">No conversation yet.</div>
  {/if}

  {#if review.hiddenResolvedCount > 0}
    <!-- Pressed, it turns Hide resolved threads off. -->
    <button
      type="button"
      class="{review.conversationFeed.length > 0 ? 'mt-3' : ''} rounded px-1 py-0.5 text-[0.6875rem] text-fg-subtle hover:text-fg compact:select-none"
      title="Show resolved threads"
      data-testid="review-conversation-hidden-resolved"
      onclick={() => review.setHideResolved(false)}
    >
      {review.hiddenResolvedCount} resolved hidden
    </button>
  {/if}

  {#if review.orphanedReplyDrafts.length > 0}
    <div class="mt-4 overflow-hidden rounded-[var(--radius-control)] border border-dashed border-border-strong" data-testid="review-orphaned-replies">
      <p class="px-3 pt-2 text-[0.6875rem] text-fg-muted">Draft replies to threads that are gone</p>
      <ReviewHeldReplies {review} replies={review.orphanedReplyDrafts} />
    </div>
  {/if}

  {#if review.sentCommentActions}
    <ReviewPRCommentField {review} />
  {/if}
</div>
