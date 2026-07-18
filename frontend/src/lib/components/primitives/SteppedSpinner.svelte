<script lang="ts">
  // Spinner for STANDING in-progress indicators (visible for minutes,
  // not a transient loading flash). Continuous rotation (animate-spin)
  // keeps compositor frame production pinned to panel refresh for as
  // long as it is visible — the standing-animation cost documented at
  // app.css `--animate-pulse` — but a plain glyph can't be stepped
  // without reading as broken. A spoked glyph can: steps(12) lands
  // every jump exactly on the next spoke position (the iOS activity
  // indicator's design), cutting updates to ~10/s. Transient spinners
  // should keep using animate-spin.
  let {
    size = 11,
    class: className = '',
    animate = true,
  }: {
    size?: number;
    class?: string;
    /** Standing surfaces pass `!lowPowerMode`; false renders the glyph static. */
    animate?: boolean;
  } = $props();

  const SPOKES = Array.from({ length: 12 }, (_, i) => ({
    angle: i * 30,
    opacity: 1 - i * 0.079,
  }));
</script>

<svg
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width="2.4"
  stroke-linecap="round"
  class="inline-block shrink-0 opacity-80 {className}"
  class:stepped-spin={animate}
  aria-hidden="true"
  data-testid="stepped-spinner"
>
  {#each SPOKES as spoke (spoke.angle)}
    <line
      x1="12"
      y1="2.5"
      x2="12"
      y2="7"
      transform="rotate({spoke.angle} 12 12)"
      opacity={spoke.opacity}
    />
  {/each}
</svg>

<style>
  .stepped-spin {
    animation: spoke-rotate 1.2s steps(12) infinite;
  }
  @keyframes spoke-rotate {
    to {
      transform: rotate(360deg);
    }
  }
</style>
