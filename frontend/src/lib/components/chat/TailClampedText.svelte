<script lang="ts">
  // Shared body primitive for the reasoning-tail rows (ThinkingBlock,
  // CompactionReasoning). While collapsed the body shows only the last 3 lines
  // of the streaming tail, bottom-anchored by the LAYOUT ENGINE: a flex column
  // with `justify-content: flex-end` packs the text to the bottom of the
  // `max-h-[3lh] overflow-hidden` box, so older lines overflow (and are
  // clipped) off the TOP while the newest line stays pinned at the bottom.
  //
  // This is intentionally pure CSS. The previous implementation pinned the tail
  // imperatively (`$effect: bodyEl.scrollTop = bodyEl.scrollHeight`) with `text`
  // as its only dependency, so it never re-ran on a width change. With
  // `whitespace-pre-wrap`, a mid-stream width oscillation (the a5a5d032 scroll-
  // spring width-reflow strand) re-wrapped the body, grew its content height,
  // and left the stale `scrollTop` scrolled UP — the tail jumped out of the
  // 3-line window until the next delta re-pinned it. The flex-end anchor is
  // re-evaluated on every reflow (width re-wrap included), so it cannot go
  // stale, and it does the work without a forced `scrollHeight` read per delta.
  // Regression: tailClampedText.browser.test.ts.
  //
  // The caller still feeds a MONOTONICALLY-GROWING live tail (the per-pane
  // smoother tail), never a re-trimmed sliding window — a shrinking/rewrapping
  // string would make the visible window jump instead of scroll. When expanded
  // the clamp and anchor are dropped entirely (plain `block`; content flows to
  // full height).

  let {
    text,
    expanded,
    id,
    testId,
    class: extraClass = '',
  }: {
    text: string;
    expanded: boolean;
    id?: string;
    testId?: string;
    class?: string;
  } = $props();
</script>

<span
  {id}
  data-testid={testId}
  class={[
    'flex-1 min-w-0 text-[0.75rem] text-fg-muted/70 italic whitespace-pre-wrap leading-relaxed',
    expanded ? 'block' : 'flex flex-col justify-end max-h-[3lh] overflow-hidden',
    extraClass || null,
  ]}
>{text}</span>
