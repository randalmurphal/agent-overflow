<script lang="ts">
  /*
   * The text of a user message, clamped when it is long.
   *
   * A message that could exceed USER_MESSAGE_CLAMP_LINES renders inside a
   * `${lines}lh` clip with the last couple of lines faded out and a
   * "Show more" / "Show less" control under it. One that could not renders
   * the same single `<p>` it always did: no clip, no observer, no effect, no
   * control. `userMessageMayClamp` is a one-sided bound (a `false` is a
   * guarantee), so that branch cannot hide a clipped message.
   *
   * The fade is a MASK, not a gradient overlay. The bubble is
   * `bg-surface-2/60` — a translucent fill over the pane background — so an
   * opaque overlay would have to reproduce
   * `color-mix(surface-2 60%, whatever is behind)`
   * and would still be wrong in one theme, and wrong again
   * the next time either token moves. Masking fades the TEXT to transparent
   * and lets the real bubble background through untouched, which is correct
   * by construction in both themes with no colour duplicated from the bubble.
   *
   * Overflow is measured, not guessed: the clip's own
   * `scrollHeight`/`clientHeight` decides whether the control appears. Mount,
   * text replacement, and collapse requests batch their reads before paint,
   * so one dirty-layout flush covers every user row mounted together. Width
   * reflows inside MessageTimeline use its existing scroll-surface observer:
   * that target sits above the virtual rows, so the toggle is settled before
   * their ResizeObserver deliveries. A clip-local observer inserted the
   * button after its row ancestor had already delivered, causing Chromium's
   * undelivered-notification loop and one-frame-stale row geometry. Standalone
   * messages retain a local observer because no virtual row exists there.
   */
  import { getContext, onDestroy } from 'svelte';
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import type { CommandSegment } from '../../utils/commandWords';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import { preservePaneScrollAnchorAt } from './preserveScrollAnchor';
  import {
    USER_MESSAGE_CLAMP_LINES,
    userMessageMayClamp,
  } from './userMessageClamp';
  import {
    USER_MESSAGE_OVERFLOW_COORDINATOR_CONTEXT,
    measureUserMessageOverflowNow,
    requestUserMessageOverflowMeasurement,
    type UserMessageOverflowCoordinator,
    type UserMessageOverflowProbe,
  } from './userMessageOverflowMeasurement';

  interface Props {
    /** The message text, attachment markers already stripped. */
    text: string;
    /** Command-word split of `text`, or empty when nothing expanded. */
    segments: readonly CommandSegment[];
    itemId: string;
    pane?: PaneSession & RowUiRegistry & ScrollHost;
  }

  let { text, segments, itemId, pane }: Props = $props();

  const clampable = $derived(userMessageMayClamp(text));

  // Expansion lives on the pane so it survives the row leaving the
  // virtualizer's window; the local fallback covers pane-less renders
  // (standalone previews, tests), exactly as DiffFileBlock does.
  let localExpanded = $state(false);
  const expanded = $derived(pane ? pane.isUserMessageExpanded(itemId) : localExpanded);

  let clipEl: HTMLParagraphElement | undefined = $state();
  let overflowResult:
    | { element: HTMLParagraphElement | undefined; text: string; overflows: boolean }
    | undefined = $state.raw();

  const collapsed = $derived(clampable && !expanded);
  const overflows = $derived(
    overflowResult !== undefined &&
      overflowResult.element === clipEl &&
      overflowResult.text === text &&
      overflowResult.overflows,
  );
  // An expanded message always offers the way back, even before the effect
  // below has run: on a remount that restored `expanded` from the registry
  // the clip is not in the DOM to measure, and a missing "Show less" would
  // strand the reader in the opened state.
  const showToggle = $derived(clampable && (expanded || overflows));
  const textDomId = $derived(
    chatRowDomId(pane, 'user-message-text', encodeURIComponent(itemId)),
  );

  const overflowCoordinator = getContext<UserMessageOverflowCoordinator | undefined>(
    USER_MESSAGE_OVERFLOW_COORDINATOR_CONTEXT,
  );
  const overflowProbe: UserMessageOverflowProbe = {
    element: () => clipEl,
    active: () => collapsed,
    apply: (next) => {
      const previous = overflowResult;
      if (
        previous !== undefined &&
        previous.element === clipEl &&
        previous.text === text &&
        previous.overflows === next
      ) {
        return;
      }
      overflowResult = { element: clipEl, text, overflows: next };
    },
  };
  const unregisterOverflowProbe = overflowCoordinator?.register(overflowProbe);
  onDestroy(() => unregisterOverflowProbe?.());

  // A timeline owns one shallow scroll-surface width observer and measures
  // every mounted user message from that delivery. A standalone message has
  // no such owner, so it keeps a local width observer. Its initial delivery
  // is state-neutral because the pre-layout request below has already set the
  // answer; later deliveries cannot feed a virtual-row observer because this
  // branch only exists outside MessageTimeline.
  $effect(() => {
    const el = clipEl;
    if (!el || overflowCoordinator) return;
    const widths = new ResizeObserver(() => {
      measureUserMessageOverflowNow(overflowProbe);
    });
    widths.observe(el);
    return () => widths.disconnect();
  });

  $effect(() => {
    const el = clipEl;
    const shouldMeasure = collapsed;
    // Re-measure same-branch text replacements as well as mount/collapse
    // transitions. Reading the value is the dependency; the batch measures
    // the post-flush DOM in the next microtask, before the browser's first
    // ResizeObserver delivery and paint.
    void text;
    if (!el || !shouldMeasure) return;
    if (overflowCoordinator) overflowCoordinator.request(overflowProbe);
    else requestUserMessageOverflowMeasurement(overflowProbe);
  });

  function toggle(): void {
    const next = !expanded;
    // The control sits BELOW the text it grows, so the text — not the button
    // — is the anchor: holding the button still would slide the message the
    // reader just decided to read off the top of the viewport.
    void preservePaneScrollAnchorAt(pane, clipEl, () => {
      if (pane) pane.setUserMessageExpanded(itemId, next);
      else localExpanded = next;
    });
  }
</script>

{#snippet content()}{#if segments.length > 0}{#each segments as segment, index (index)}{#if segment.command}<span
        class="text-accent"
        data-testid="user-message-command">{segment.text}</span>{:else}{segment.text}{/if}{/each}{:else}{text}{/if}{/snippet}

<!-- wrap-anywhere, not break-words: `overflow-wrap: break-word` doesn't
     lower a line's min-content width, and the bubble is a shrink-to-fit
     flex child whose fit-content sizing floors at min-content — pasted
     plaintext tables (NBSP-padded cells, unbroken border runs) blew the
     bubble past the pane edge instead of wrapping. `anywhere` counts the
     break opportunities in min-content, so the 82% cap holds.
     Guard: userMessageOverflow.browser.test.ts -->
{#if clampable}
  <p
    bind:this={clipEl}
    id={textDomId}
    data-testid="user-message-summary"
    data-clamped={collapsed && overflows ? 'true' : undefined}
    class={[
      'whitespace-pre-wrap wrap-anywhere',
      collapsed && 'overflow-hidden',
      collapsed && overflows && 'user-message-clamp-fade',
    ]}
    style={collapsed ? `max-height: ${USER_MESSAGE_CLAMP_LINES}lh` : undefined}
  >{@render content()}</p>
  {#if showToggle}
    <button
      type="button"
      onclick={toggle}
      aria-expanded={expanded}
      aria-controls={textDomId}
      data-testid="user-message-clamp-toggle"
      class="-ml-1 mt-1 inline-flex rounded px-1 py-0.5 text-[0.6875rem] text-fg-hint/70
             transition-colors cursor-pointer hover:bg-surface-2/40 hover:text-fg-muted
             focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35"
    >{expanded ? 'Show less' : 'Show more'}</button>
  {/if}
{:else}
  <p class="whitespace-pre-wrap wrap-anywhere" data-testid="user-message-summary"
  >{@render content()}</p>
{/if}

<style>
  /* Fades the glyphs themselves, so the bubble's own translucent background
     shows through unchanged — see the header. Both spellings: WebKitGTK is
     the Linux shell's engine and still prefers the prefixed property. */
  .user-message-clamp-fade {
    -webkit-mask-image: linear-gradient(to bottom, #000 calc(100% - 2.5rem), transparent);
    mask-image: linear-gradient(to bottom, #000 calc(100% - 2.5rem), transparent);
  }
</style>
