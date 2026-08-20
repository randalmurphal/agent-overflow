<script lang="ts">
  // The map's fold/reveal wrapper (RUN-MAP §9.8, §10): `grid-template-rows`
  // 0fr ⇄ 1fr, which animates a region of unknown height without measuring it.
  //
  // The wrapper stays mounted in BOTH states on purpose — a CSS transition
  // needs its from-value present before the value changes, so a wrapper that
  // appeared with the content would snap open instead of revealing. What it
  // wraps must therefore be cheap to leave mounted: the wave body builds no
  // segments while closed, and a group chip renders no entries.
  //
  // Motion gate: app.css's `prefers-reduced-motion` reset covers the OS
  // preference, but NOT the app's low-power setting — the second half of
  // `motionReduced()`, and the one the app owns. §10 gates every structural
  // motion on the full gate, so the transition class is dropped rather than
  // zeroed: low power means the fold applies instantly.
  //
  // §9.8 gates it a SECOND way, and that half is a scroll rule rather than a
  // motion one: a fold outside the viewport applies instantly. The 200ms height
  // transition otherwise spends 199 of those milliseconds moving the document
  // AFTER the anchor compensation that was supposed to cancel it (§9.7 measures
  // once, at the flush), so an auto-fold above a reader who is not following
  // drifts their viewport for the rest of the animation.
  //
  // The decision is made in an `$effect.pre` keyed on `open`: pre-effects run
  // before the DOM update, so the rect it reads is the layout the fold is about
  // to change, which is the only moment "is this region on screen" has the
  // answer §9.8 is asking for. One rect read per toggle — no per-frame work,
  // and no write of any kind (§9.1's writer set is untouched).

  import type { Snippet } from 'svelte';
  import { motionReduced } from '../../utils/reducedMotion';
  import { foldAnimates } from './runMapGeometry';
  import { requireWorkflowsOverlayScroller } from './overlayScroller';

  interface Props {
    open: boolean;
    children: Snippet;
    testId?: string;
  }
  let { open, children, testId }: Props = $props();

  const scrollerOf = requireWorkflowsOverlayScroller();

  let regionEl = $state<HTMLElement | null>(null);
  // Plain `$state`, decided per toggle. A `$derived` cannot express it: the
  // answer depends on geometry read at a specific moment, not on any signal.
  let animated = $state(false);

  // $derived so the gate stays live when the low-power setting flips but a
  // settings save that didn't move the flag can't wake every mounted fold
  // (the settings object is replaced wholesale per save, and each wake costs
  // a getBoundingClientRect).
  const reduced = $derived(motionReduced());

  $effect.pre(() => {
    // `open` is the trigger and `reduced` keeps the gate live; both are read
    // tracked, on purpose.
    void open;
    animated = foldAnimates(scrollerOf(), regionEl, reduced);
  });
</script>

<div
  bind:this={regionEl}
  class={['grid', animated ? 'transition-[grid-template-rows] duration-200 ease-out' : ''].filter(Boolean).join(' ')}
  style:grid-template-rows={open ? '1fr' : '0fr'}
  data-testid={testId}
  data-open={open}
  data-fold-animated={animated}
>
  <div class="min-h-0 overflow-hidden">
    {@render children()}
  </div>
</div>
