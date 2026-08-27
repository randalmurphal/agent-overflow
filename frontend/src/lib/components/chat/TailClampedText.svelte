<script lang="ts">
  // Shared body primitive for the reasoning-tail rows (ThinkingBlock,
  // CompactionReasoning). While collapsed the body shows only the last 3 lines
  // of the streaming tail, bottom-anchored by the LAYOUT ENGINE: a flex column
  // with `justify-content: flex-end` packs the text to the bottom of the
  // `max-h-[3lh] overflow-hidden` box, so older lines overflow (and are
  // clipped) off the TOP while the newest line stays pinned at the bottom.
  //
  // The ANCHOR is intentionally pure CSS. The previous implementation pinned
  // the tail imperatively (`$effect: bodyEl.scrollTop = bodyEl.scrollHeight`)
  // with `text` as its only dependency, so it never re-ran on a width change.
  // With `whitespace-pre-wrap`, a mid-stream width oscillation (the a5a5d032
  // scroll-spring width-reflow strand) re-wrapped the body, grew its content
  // height, and left the stale `scrollTop` scrolled UP — the tail jumped out
  // of the 3-line window until the next delta re-pinned it. The flex-end
  // anchor is re-evaluated on every reflow (width re-wrap included), so it
  // cannot go stale, and it does the work without a forced `scrollHeight`
  // read per delta. Regression: tailClampedText.browser.test.ts.
  //
  // On TOP of that anchor sits one presentational enhancement: the
  // line-slide FLIP (see the animation section below), which turns the
  // anchor's instant one-line re-pack at each line boundary into a short
  // transition. It is decoration over the CSS truth — every guard path
  // falls back to exactly the un-animated anchor, so correctness never
  // depends on it.
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
  import { motionReduced } from '../../utils/reducedMotion';
  import {
    SLIDE_MS,
    slideDecision,
    transformTranslateY,
    type SlideObservation,
  } from './tailSlide';
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
  let innerEl: HTMLSpanElement | undefined = $state();

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

  // ── Line-slide animation ─────────────────────────────────────────────
  // FLIP on the inner wrapper: when the flex-end anchor re-packs the
  // content one line up at a line boundary, invert the jump with a
  // transform in the same frame the layout moved (ResizeObserver
  // callbacks run after layout, before paint, so nothing flashes), then
  // release it to rest — the newest line rises from under the bottom
  // clip edge, which is exactly what a smooth ticker scroll looks like.
  //
  // The RO watches the inner wrapper and the clamp box, whose heights
  // only change at line boundaries (and reflows) — zero per-reveal-tick
  // cost, no forced layout reads on the streaming path. WHICH
  // observations animate — and the full recalibrate-and-snap matrix
  // (mount, width reflow, box-height change incl. the expanded flip,
  // whole-window discontinuity, hidden ancestor) — is tailSlide.ts's
  // `slideDecision`, unit-tested there. `slideMemory === null` is the
  // one reset sentinel: the RO's next delivery recalibrates.
  //
  // The motion itself is one fill-none `Element.animate()` from the
  // inverted offset to rest. Spike-verified (2026-08-06, Playwright
  // Chromium + WebKit): an animation created inside an RO callback
  // applies before the first paint after the layout move — same no-flash
  // guarantee as an inline style write, without its costs: at finish the
  // effect stops applying on its own (the element carries no inline
  // residue, ever), and every guard path is a single cancel() instead of
  // a transition/transform unwind plus rAF bookkeeping. (Transform
  // transitions and WAAPI both run on the compositor; that is not the
  // difference. Note the animated inner promotes a layer sized to the
  // FULL windowed content, and `contain: paint` on the box measurably
  // does not bound it — tiled rasterization is what keeps the real cost
  // near the visible tiles.)
  let slideMemory: SlideObservation | null = null;
  let slideAnim: Animation | null = null;

  function clearSlide(): void {
    slideAnim?.cancel();
    slideAnim = null;
  }

  $effect(() => {
    const inner = innerEl;
    const outer = el;
    if (!inner || !outer || typeof ResizeObserver === 'undefined') return;
    // Geometry comes from the RO entries themselves: border-box,
    // fractional, transform-independent — exactly SlideObservation's
    // contract (the 19.5px line height and sub-pixel width oscillations
    // both round away in offset* ints). A delivery only carries the
    // element that changed, so last-known sizes are held per element.
    // The old shape took two getBoundingClientRect reads per delivery
    // for numbers the observer had already measured — 68ms of gBCR at
    // this site over a 40s 3-pane storm (2026-08-26).
    const sizes = new Map<Element, { h: number; w: number }>();
    const ro = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const box = entry.borderBoxSize?.[0];
        sizes.set(
          entry.target,
          box
            ? { h: box.blockSize, w: box.inlineSize }
            : { h: entry.contentRect.height, w: entry.contentRect.width },
        );
      }
      const innerSize = sizes.get(inner);
      const outerSize = sizes.get(outer);
      if (!innerSize || !outerSize) return;
      // The live interpolated translateY — a slide landing mid-slide
      // compounds from where the text IS, so the start position cannot
      // come from a remembered target. Resolved from computed style ONLY
      // while a slide is actually running; at rest the transform is
      // identically none (fill-none animation) and the style resolve is
      // skipped on the common line-boundary path.
      const liveTy =
        slideAnim && slideAnim.playState === 'running'
          ? transformTranslateY(getComputedStyle(inner).transform)
          : 0;
      const decision = slideDecision(
        slideMemory,
        { innerH: innerSize.h, innerW: innerSize.w, outerH: outerSize.h },
        liveTy,
      );
      slideMemory = decision.memory;
      if (decision.kind === 'clear') {
        clearSlide();
        return;
      }
      // Gate checked after the baseline update so a low-power toggle
      // mid-stream leaves the geometry bookkeeping calibrated.
      if (decision.kind !== 'slide' || motionReduced()) return;
      // startPx already carries the in-flight displacement (read above,
      // before this cancel), so replacing the animation is seamless —
      // nothing paints between cancel and animate in the same callback.
      slideAnim?.cancel();
      slideAnim = inner.animate(
        [{ transform: `translateY(${decision.startPx}px)` }, { transform: 'translateY(0px)' }],
        { duration: SLIDE_MS, easing: 'ease-out' },
      );
    });
    ro.observe(inner);
    ro.observe(outer);
    return () => {
      ro.disconnect();
      clearSlide();
    };
  });

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
      // The content was REPLACED (retained-tail drop → trimmed summary).
      // Any clip change is a swap, not a slide — recalibrate, and drop an
      // in-flight slide even if the swap lands height-identical (the one
      // swap shape the RO cannot see). The expanded flip needs no arm
      // here: it moves the clamp box, and the RO watches the box.
      slideMemory = null;
      clearSlide();
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
        const node = innerEl?.firstChild;
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
<!-- The inner wrapper exists for the slide animation: it is the element
     whose height tracks the content (the RO's line-boundary signal) and the
     transform target for the FLIP. It must be the outer flex column's ONLY
     child — with `whitespace-pre-wrap`, stray template whitespace inside
     the flex container would become a real anonymous flex item. -->
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
><span bind:this={innerEl} class="block">{rendered}</span></span>
