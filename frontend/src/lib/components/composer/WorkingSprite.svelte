<script lang="ts">
  // The animated working-indicator sprite: one frame of a horizontal
  // strip PNG, shown in the LED cluster's slot in the activity rail.
  //
  // The strip translates inside a one-frame clipping window, stepped
  // once per frame. transform is compositable, so Blink runs the whole
  // thing off the main thread: this component has no timer, no lifecycle
  // JS, and costs zero style recalcs while it plays.
  //
  // It used to advance frames with an inline background-position-x write
  // from its own wall-clock timer. That was the single most expensive
  // thing in the renderer — background-position is not compositable, so
  // each of its ~25 writes/sec repainted the whole document. Measured
  // 2026-08-23 over 5s at 25 frames/s: 163.0ms of main-thread work then,
  // 0.0ms now. Promoting the sprite to its own layer and keeping the JS
  // write was also tried and only reached 95.4ms.
  //
  // Frame width is snapped to whole device pixels in app.css, which is
  // what keeps this pixel-identical to the old write rather than
  // slightly soft — see the note there.
  //
  // Phase is wall-clock, so remounts and pane switches land mid-cycle
  // instead of restarting: utils/ambientPhase.ts pins the animation's
  // startTime the moment it begins, the same beat every other ambient
  // indicator shares.
  //
  // The strip is keyed on the sprite id, and that is load-bearing rather
  // than tidiness. Swapping art changes only the duration and step count
  // inside the shorthand, never `animation-name`, so Blink updates the
  // running animation IN PLACE: no `animationstart` fires, the phase
  // module has already recorded that Animation object as aligned, and
  // the startTime pinned for the old period silently becomes wrong for
  // the new one. Keying forces a fresh element, hence a fresh animation
  // and a fresh pin. Art changes at most once per turn, so the remount
  // is one span.
  //
  // Freeze paths, all frame 0 with no timer to stop: `animate` false
  // (low-power mode) drops the animation, and OS reduced-motion is
  // collapsed by the global reset in app.css. A hidden document needs no
  // handling at all — Chromium does not tick a composited animation for
  // a hidden page, and the document timeline keeps advancing, so the
  // sprite is on the correct frame the instant it comes back.
  import type { SpinnerSprite } from '../../spinners/catalog';

  interface Props {
    sprite: SpinnerSprite;
    /** False drops the animation and rests at frame 0 (low-power mode). */
    animate?: boolean;
    /**
     * True (the default) applies the rail's negative margin-block so the
     * oversized sprite fits the activity rail's single-row height
     * contract. Settings previews pass false and take the full height.
     */
    inRail?: boolean;
  }

  let { sprite, animate = true, inRail = true }: Props = $props();

  let aspect = $derived(sprite.frameWidth / sprite.frameHeight);
  // One pass over the whole strip; steps() lands on each frame in turn
  // and never shows the final `to` value, so N steps = N frames.
  let stripStyle = $derived(
    `background-image:url('${sprite.src}')` +
      (animate
        ? `;animation:working-sprite-run ${sprite.frames * sprite.frameMs}ms steps(${sprite.frames}) infinite`
        : ''),
  );
</script>

<span
  class="working-sprite-slot {inRail ? 'working-sprite-slot-rail' : ''}"
  aria-hidden="true"
  data-testid="activity-rail-sprite"
>
  <span
    class="working-sprite"
    style="--working-sprite-aspect:{aspect};--working-sprite-frames:{sprite.frames}"
    data-sprite-id={sprite.id}
  >
    {#key sprite.id}
      <span class="working-sprite-strip" style={stripStyle}></span>
    {/key}
  </span>
</span>
