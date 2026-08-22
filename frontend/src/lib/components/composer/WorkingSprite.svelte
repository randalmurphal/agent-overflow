<script lang="ts">
  // The animated working-indicator sprite: one frame of a horizontal
  // strip PNG, shown in the LED cluster's slot in the activity rail.
  //
  // Frame advance is a JS timer writing background-position-x inline —
  // NEVER a CSS animation, for the ambientTicker reason (a running CSS
  // animation ticks style recalc every vsync even with steps(); see
  // utils/ambientTicker.ts's header). It does not ride the ambient
  // ticker itself because sprite cadences are the art's own (30ms happy
  // cat, 200ms typing robot) and quantizing them onto the shared 125ms
  // slot grid would wreck native timing; instead each mounted sprite —
  // at most one per composer, alive only while a turn runs — owns one
  // wall-clock-aligned setTimeout at its own frameMs. The write happens
  // only when the frame index actually changes, so cost is exactly one
  // leaf recalc per frame flip and zero when idle or hidden.
  //
  // Frame selection is wall-clock (floor(now / frameMs) % frames), so
  // remounts and pane switches land mid-cycle instead of restarting,
  // matching every other ambient indicator's shared phase.
  //
  // Freeze paths (frame 0, no timer): `animate` false (low-power mode),
  // OS reduced-motion, and document.hidden (with a visibilitychange
  // resync so the sprite never shows a stale frame on return).

  import { onMount } from 'svelte';
  import { prefersReducedMotion, reducedMotionQuery } from '../../utils/reducedMotion';
  import type { SpinnerSprite } from '../../spinners/catalog';

  interface Props {
    sprite: SpinnerSprite;
    /** False drops the timer and rests at frame 0 (low-power mode). */
    animate?: boolean;
    /**
     * True (the default) applies the rail's negative margin-block so the
     * oversized sprite fits the activity rail's single-row height
     * contract. Settings previews pass false and take the full height.
     */
    inRail?: boolean;
  }

  let { sprite, animate = true, inRail = true }: Props = $props();

  let el: HTMLSpanElement | undefined;
  let shownFrame = -1;
  let timer: ReturnType<typeof setTimeout> | null = null;

  function stopTimer(): void {
    if (timer !== null) clearTimeout(timer);
    timer = null;
  }

  function writeFrame(frame: number): void {
    if (el === undefined || frame === shownFrame) return;
    shownFrame = frame;
    // calc over the inherited size vars keeps the offset correct across
    // root font-size changes without recomputing pixel math here.
    el.style.backgroundPositionX =
      frame === 0 ? '' : `calc(${-frame} * var(--working-sprite-fw))`;
  }

  function tick(): void {
    stopTimer();
    if (!animate || prefersReducedMotion()) {
      writeFrame(0);
      return;
    }
    if (document.hidden) {
      // Nothing renders; the visibilitychange listener re-enters tick().
      return;
    }
    const now = Date.now();
    writeFrame(Math.floor(now / sprite.frameMs) % sprite.frames);
    timer = setTimeout(tick, sprite.frameMs - (now % sprite.frameMs) || sprite.frameMs);
  }

  // Re-arm on sprite identity or animate changes; reset the frame
  // memory so the new strip never keeps the old strip's offset.
  $effect(() => {
    void sprite;
    void animate;
    shownFrame = -1;
    tick();
    return stopTimer;
  });

  onMount(() => {
    const resync = (): void => tick();
    reducedMotionQuery()?.addEventListener('change', resync);
    document.addEventListener('visibilitychange', resync);
    return () => {
      stopTimer();
      reducedMotionQuery()?.removeEventListener('change', resync);
      document.removeEventListener('visibilitychange', resync);
    };
  });

  let aspect = $derived(sprite.frameWidth / sprite.frameHeight);
</script>

<span
  class="working-sprite-slot {inRail ? 'working-sprite-slot-rail' : ''}"
  aria-hidden="true"
  data-testid="activity-rail-sprite"
>
  <span
    bind:this={el}
    class="working-sprite"
    style="background-image:url('{sprite.src}');--working-sprite-aspect:{aspect};--working-sprite-frames:{sprite.frames}"
    data-sprite-id={sprite.id}
  ></span>
</span>
