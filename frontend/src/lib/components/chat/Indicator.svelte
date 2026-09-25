<script lang="ts">
  import type { IndicatorState } from './rowState';

  /*
   * Text-free state dot for tool-call / think rows. Single source of
   * truth for visual run-state on a row: callers pass one of seven
   * `state` values and the dot renders the matching visual (or
   * nothing for `null` — the absence of a dot is the positive
   * "idle / success" signal). No text labels, no exit-code chips,
   * no "RUNNING" / "Failed" / "Stopped" annotations — those live in
   * `RowError.svelte` when an error sub-line is warranted.
   *
   * `null` is the redesigned success state. Error/declined detail
   * belongs in `RowError.svelte`.
   *
   * `backgrounded` uses three small staggered dots so a running
   * backgrounded launch is visually distinct from a foreground
   * running row at a glance. `settled` is the same box with the dots
   * hidden: a launch that settles loses its dots without moving
   * anything beside them. It is hidden from screen readers, as `null`.
   */

  type NonNullState = Exclude<IndicatorState, null>;

  const ARIA_BY_STATE: Record<Exclude<NonNullState, 'settled'>, string> = {
    running: 'Running',
    backgrounded: 'Backgrounded',
    parked: 'Parked',
    error: 'Errored',
    declined: 'Declined',
  };

  // Single-dot states share geometry; only the fill and the pulse flag
  // differ. `parked` is the hollow, still ring: the agent is alive but
  // doing nothing until its background command reports. `backgrounded`
  // and `settled` render separately because their box is three smaller
  // dots.
  const SINGLE_DOT_BY_STATE: Record<Exclude<NonNullState, 'backgrounded' | 'settled'>, { bg: string; pulse: boolean }> = {
    running: { bg: 'bg-accent', pulse: true },
    parked: { bg: 'border border-accent/70 bg-transparent', pulse: false },
    error: { bg: 'bg-error', pulse: false },
    declined: { bg: 'bg-warning', pulse: false },
  };

  // Stagger offsets for the backgrounded three-dot variant.
  // `ambient-pulse-s2`/`-s4` set a negative `animation-delay` in
  // app.css, running those two dots one and two slots off the first so
  // the three never breathe in unison. Shifts stay on the ambient 125ms
  // slot grid, so all three land on values the stepped waveform already
  // has; an off-grid offset would show a value between steps.
  const BG_DOT_SHIFTS = ['', 'ambient-pulse-s2', 'ambient-pulse-s4'] as const;

  interface Props {
    /**
     * Run state. `null` (or absent) renders nothing — that's the
     * idle / success signal. The six non-null states each map to a
     * specific visual:
     *   - `running`     pulsing accent dot
     *   - `backgrounded` three staggered pulsing accent dots
     *   - `settled`     the `backgrounded` box, dots hidden
     *   - `parked`      static hollow accent ring
     *   - `error`       static red dot
     *   - `declined`    static amber dot
     */
    state: IndicatorState;
    /**
     * Override the aria-label. Defaults to the run-state name so
     * screen readers announce "running" / "errored" / etc. without
     * the row needing to wire it.
     */
    ariaLabel?: string;
    class?: string;
  }

  let { state, ariaLabel, class: className = '' }: Props = $props();

  const label = $derived(ariaLabel ?? (state && state !== 'settled' ? ARIA_BY_STATE[state] : ''));
  const singleDot = $derived(
    state && state !== 'backgrounded' && state !== 'settled' ? SINGLE_DOT_BY_STATE[state] : null,
  );
</script>

{#if singleDot}
  <span
    class="inline-block h-1.5 w-1.5 shrink-0 rounded-full {singleDot.bg} {singleDot.pulse ? 'animate-pulse' : ''} {className}"
    data-testid="indicator"
    data-state={state}
    role="status"
    aria-label={label}
  ></span>
{:else if state === 'backgrounded' || state === 'settled'}
  {@const settled = state === 'settled'}
  <span
    class="inline-flex shrink-0 items-center gap-[3px] {className}"
    data-testid="indicator"
    data-state={state}
    role={settled ? undefined : 'status'}
    aria-label={settled ? undefined : label}
    aria-hidden={settled ? 'true' : undefined}
  >
    {#each BG_DOT_SHIFTS as shiftClass}
      <span class="h-[3.5px] w-[3.5px] rounded-full bg-accent {settled ? 'invisible' : 'animate-pulse'} {shiftClass}"></span>
    {/each}
  </span>
{/if}
