<script module lang="ts">
  // Single definition of the meter button chrome so the side-by-side
  // header meters can't drift apart on size, hover, or focus ring.
  // The 2rem flex button centers MeterRing's 1.75rem face.
  export const METER_BUTTON_CLASS =
    'inline-flex h-8 w-8 items-center justify-center bg-transparent border-none p-0 cursor-help focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded-full hover:bg-surface-2/30 transition-colors';
</script>

<script lang="ts">
  // Shared ring face for ContextWindowMeter and RateLimitMeter — one
  // geometry definition so the header meters always line up visually.
  //
  // The label is SVG <text> in the ring's own viewBox, not an HTML
  // span centered over the ring. The app zoom (utils/zoom.ts) makes
  // the root font-size fractional at every non-default step, and
  // Blink rounds a line box's ascent/descent to whole CSS pixels
  // before grid-fitting glyphs to the device pixel grid — so an HTML
  // label drifts up to a device pixel off the ring's true fractional
  // center, clearly visible on ~8px digits (worst at high DPR). SVG
  // text shares the ring's coordinate system and scales geometrically
  // with it, so the label cannot drift.
  //
  // `percentage` (0–100) is clamped and NaN/Infinity-guarded here so a
  // wire glitch can't produce a NaN or negative dashoffset (which
  // renders as a no-arc or longer-than-full circle in some browsers).
  //
  // `showArc={false}` renders just the grey track: RateLimitMeter's
  // pre-first-update ring must read as "no data yet", distinct from a
  // genuine 0% fill (which would also leak a rounded-linecap dot).
  let {
    label,
    percentage,
    strokeClass,
    showArc = true,
  }: {
    label: string;
    percentage: number;
    /** Tailwind stroke-* class for the progress arc. */
    strokeClass: string;
    showArc?: boolean;
  } = $props();

  const RADIUS = 9.75;
  const CIRCUMFERENCE = 2 * Math.PI * RADIUS;
  // Label-to-ring ratio the meters have always had (8.5px text in a
  // 28px ring at default zoom), expressed in the 24-unit viewBox.
  const LABEL_FONT_SIZE = (0.53125 / 1.75) * 24;

  let clamped = $derived(
    Number.isFinite(percentage) ? Math.max(0, Math.min(100, percentage)) : 0,
  );
  let dashOffset = $derived(CIRCUMFERENCE - (clamped / 100) * CIRCUMFERENCE);
</script>

<!-- The circles rotate -90° (arc origin at 12 o'clock) via the <g>;
     the label sits outside the group so it does not rotate. -->
<svg class="h-7 w-7" viewBox="0 0 24 24" aria-hidden="true">
  <g transform="rotate(-90 12 12)">
    <circle
      cx="12" cy="12" r={RADIUS}
      fill="none"
      stroke-width="3"
      class="stroke-text-secondary/20"
    />
    {#if showArc}
      <circle
        cx="12" cy="12" r={RADIUS}
        fill="none"
        stroke-width="3"
        stroke-linecap="round"
        stroke-dasharray={CIRCUMFERENCE}
        stroke-dashoffset={dashOffset}
        class={strokeClass}
      />
    {/if}
  </g>
  <text
    x="12" y="12"
    text-anchor="middle"
    dominant-baseline="central"
    font-size={LABEL_FONT_SIZE}
    text-rendering="geometricPrecision"
    class="fill-text-secondary font-semibold tabular-nums"
  >{label}</text>
</svg>
