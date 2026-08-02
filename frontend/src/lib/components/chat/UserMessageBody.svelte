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
   * `bg-surface-2/60` — a translucent fill over the pane background, and over
   * the target-flash glow while a jump animates it — so an opaque overlay
   * would have to reproduce `color-mix(surface-2 60%, whatever is behind)`
   * and would still be wrong in one theme, wrong mid-flash, and wrong again
   * the next time either token moves. Masking fades the TEXT to transparent
   * and lets the real bubble background through untouched, which is correct
   * by construction in both themes with no colour duplicated from the bubble.
   *
   * Overflow is measured, not guessed: the clip's own
   * `scrollHeight`/`clientHeight` decides whether the control appears, re-read
   * whenever the clip's width changes (a divider drag re-wraps the text, and a
   * message that fit at one width does not at another). The width comes from
   * Svelte's `bind:clientWidth` on the clip — a row-local observer that exists
   * only on the clamped branch, never a global one — and the read is bounded:
   * while collapsed the clip's height is pinned by the clamp, so the state
   * this effect writes cannot feed back into the width it depends on.
   */
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { CommandSegment } from '../../utils/commandWords';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import { preservePaneScrollAnchorAt } from './preserveScrollAnchor';
  import {
    USER_MESSAGE_CLAMP_EPSILON_PX,
    USER_MESSAGE_CLAMP_LINES,
    userMessageMayClamp,
  } from './userMessageClamp';

  interface Props {
    /** The message text, attachment markers already stripped. */
    text: string;
    /** Command-word split of `text`, or empty when nothing expanded. */
    segments: readonly CommandSegment[];
    itemId: string;
    pane?: ThreadPane;
  }

  let { text, segments, itemId, pane }: Props = $props();

  const clampable = $derived(userMessageMayClamp(text));

  // Expansion lives on the pane so it survives the row leaving the
  // virtualizer's window; the local fallback covers pane-less renders
  // (standalone previews, tests), exactly as DiffFileBlock does.
  let localExpanded = $state(false);
  const expanded = $derived(pane ? pane.isUserMessageExpanded(itemId) : localExpanded);

  let clipEl: HTMLParagraphElement | undefined = $state();
  let clipWidth = $state(0);
  let overflows = $state(false);

  const collapsed = $derived(clampable && !expanded);
  // An expanded message always offers the way back, even before the effect
  // below has run: on a remount that restored `expanded` from the registry
  // the clip is not in the DOM to measure, and a missing "Show less" would
  // strand the reader in the opened state.
  const showToggle = $derived(clampable && (expanded || overflows));
  const textDomId = $derived(
    chatRowDomId(pane, 'user-message-text', encodeURIComponent(itemId)),
  );

  $effect(() => {
    if (!collapsed) return;
    const el = clipEl;
    // Depend on the measured width: re-wrapping is the only thing that can
    // change the answer for a fixed text.
    const width = clipWidth;
    if (!el || width <= 0) return;
    overflows = el.scrollHeight - el.clientHeight > USER_MESSAGE_CLAMP_EPSILON_PX;
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
    bind:clientWidth={clipWidth}
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
