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
  // Callers feed a MONOTONICALLY-GROWING live tail (the per-pane smoother
  // tail) — never a pre-trimmed sliding window, whose moving start offset
  // would re-wrap (and visibly jump) the visible lines on every delta. The
  // component bounds its own layout cost instead: clipping bounds what is
  // painted, not what is laid out, so an unbounded tail re-line-breaks the
  // ENTIRE thinking text on every ~50Hz reveal tick. While collapsed, the
  // rendered text is windowed at WRAP-STABLE offsets only (hard newlines, or
  // a measured rendered line start for a monster single paragraph — see
  // tailWindow.ts), which keeps the visible lines pixel-identical while
  // capping per-tick layout at the window cap (~8k chars) instead of the
  // whole accumulated tail. Append-detection
  // sentinels reset the window on the non-append transitions — the swap to
  // the rune-trimmed summary when the retained tail is dropped (offscreen
  // prune, budget eviction, post-settle summary overwrite; see
  // threadStreamingReveal.svelte.ts). When expanded the clamp, anchor, and
  // window are all dropped (plain `block`; full text flows to full height).
  import { untrack } from 'svelte';
  import {
    TAIL_CLAMP_LINES,
    TAIL_WINDOW_CAP_CHARS,
    TAIL_WINDOW_KEEP_LINES,
    TAIL_WINDOW_MEASURE_RETRY_CHARS,
    TAIL_WINDOW_MIN_KEEP_CHARS,
    isMonotonicAppend,
    measuredLineStartOffset,
    newlineCutOffset,
  } from './tailWindow';

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

  let el: HTMLSpanElement | undefined = $state();

  /** Start of the wrap-stable window into `text` while collapsed. */
  let cutOffset = $state(0);

  // Non-reactive bookkeeping for isMonotonicAppend (which detects the
  // non-append transitions — the swap to the shorter rune-trimmed
  // summary when the retained tail is dropped — and resets the window)
  // and for throttling failed measurements.
  let prevLen = 0;
  let prevLastCharCode = 0;
  let cutFirstCharCode = 0;
  let measureFloor = 0;

  // Seed the window before the first render: a windowing remount can
  // arrive with the full retained tail (tens of KB), and starting at
  // cutOffset 0 would line-break the entire string once before the
  // effect below installs a cut. Newline cuts need no geometry, so they
  // can run at init; the monster-single-paragraph case still pays one
  // full first layout and gets its measured cut after the first flush.
  // untrack: the INITIAL prop values are exactly what a mount seed
  // wants — later changes are the effect's job.
  const initialText = untrack(() => text);
  if (!untrack(() => expanded) && initialText.length > TAIL_WINDOW_CAP_CHARS) {
    const initialCut = newlineCutOffset(initialText, 0, TAIL_WINDOW_MIN_KEEP_CHARS);
    if (initialCut !== null) {
      cutOffset = initialCut;
      cutFirstCharCode = initialText.charCodeAt(initialCut);
    }
  }

  const rendered = $derived(expanded || cutOffset === 0 ? text : text.slice(cutOffset));

  // Depends on `text` and `expanded` only — `cutOffset` reads go through
  // `untrack` so the effect's own cut writes can't re-trigger it.
  $effect(() => {
    const t = text;
    const renderedCut = untrack(() => cutOffset);
    let cut = renderedCut;

    if (!isMonotonicAppend(t, prevLen, prevLastCharCode, cut, cutFirstCharCode)) {
      cut = 0;
      measureFloor = 0;
    }
    prevLen = t.length;
    prevLastCharCode = t.length > 0 ? t.charCodeAt(t.length - 1) : 0;

    if (!expanded && t.length - cut > TAIL_WINDOW_CAP_CHARS) {
      const nlCut = newlineCutOffset(t, cut, TAIL_WINDOW_MIN_KEEP_CHARS);
      if (nlCut !== null) {
        cut = nlCut;
      } else if (cut === renderedCut && t.length >= measureFloor) {
        // The whole advanceable region is one giant paragraph — cut at
        // a measured rendered line start instead. Effects run after DOM
        // flush, so the text node currently renders t.slice(renderedCut)
        // and the measured offset is relative to it — which is why a
        // run that just reset the cut must not measure (the DOM still
        // shows the pre-reset window until the next flush).
        const node = el?.firstChild;
        if (el && node instanceof Text) {
          const lineHeight = Number.parseFloat(getComputedStyle(el).lineHeight);
          const rel = measuredLineStartOffset(node, TAIL_WINDOW_KEEP_LINES, lineHeight);
          if (rel === null) measureFloor = t.length + TAIL_WINDOW_MEASURE_RETRY_CHARS;
          else cut += rel;
        }
      }
    }

    if (cut !== renderedCut) {
      cutFirstCharCode = cut > 0 ? t.charCodeAt(cut) : 0;
      cutOffset = cut;
    }
  });
</script>

<!-- data-collapsed-lines tells an enclosing activity run's height cap that
     this body occupies up to that many lines while collapsed — expanding it
     adds only the height beyond the clamp (utils/activityRunClip.ts). Present
     in both states: the cap reads it while the row is EXPANDED, including a
     row that remounts already-expanded and never renders its clamp. -->
<span
  bind:this={el}
  {id}
  data-testid={testId}
  data-collapsed-lines={TAIL_CLAMP_LINES}
  class={[
    'flex-1 min-w-0 text-[0.75rem] text-fg-muted/70 italic whitespace-pre-wrap leading-relaxed',
    expanded ? 'block' : 'flex flex-col justify-end max-h-[3lh] overflow-hidden',
    extraClass || null,
  ]}
>{rendered}</span>
