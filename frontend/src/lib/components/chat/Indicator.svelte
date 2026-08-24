<script lang="ts">
  /*
   * Text-free state dot for tool-call / think rows. Single source of
   * truth for visual run-state on a row: callers pass one of five
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
   * running row at a glance.
   */

  type State = 'running' | 'backgrounded' | 'error' | 'declined' | null;
  type NonNullState = Exclude<State, null>;

  const ARIA_BY_STATE: Record<NonNullState, string> = {
    running: 'Running',
    backgrounded: 'Backgrounded',
    error: 'Errored',
    declined: 'Declined',
  };

  // Single-dot states share geometry; only the bg color and the
  // pulse flag differ. `backgrounded` renders separately because its
  // visual is three smaller dots.
  const SINGLE_DOT_BY_STATE: Record<Exclude<NonNullState, 'backgrounded'>, { bg: string; pulse: boolean }> = {
    running: { bg: 'bg-accent', pulse: true },
    error: { bg: 'bg-error', pulse: false },
    declined: { bg: 'bg-warning', pulse: false },
  };

  // Stagger offsets for the backgrounded three-dot variant.
  // ambient-pulse-s2/-s4 carry an `animation-delay` in app.css that
  // shifts those dots 2 and 4 slots (250ms / 500ms) along the shared
  // `ambient-pulse` keyframes. Shifts stay on the animation's own
  // 125ms step grid so all three dots change on the same instants;
  // an off-grid offset would land a dot mid-step and read as drift.
  // Phase is wall-clock, not mount-time — see utils/ambientPhase.ts.
  const BG_DOT_SHIFTS = ['', 'ambient-pulse-s2', 'ambient-pulse-s4'] as const;

  interface Props {
    /**
     * Run state. `null` (or absent) renders nothing — that's the
     * idle / success signal. The four non-null states each map to a
     * specific visual:
     *   - `running`     pulsing accent dot
     *   - `backgrounded` three staggered pulsing accent dots
     *   - `error`       static red dot
     *   - `declined`    static amber dot
     */
    state: State;
    /**
     * Override the aria-label. Defaults to the run-state name so
     * screen readers announce "running" / "errored" / etc. without
     * the row needing to wire it.
     */
    ariaLabel?: string;
    class?: string;
  }

  let { state, ariaLabel, class: className = '' }: Props = $props();

  const label = $derived(ariaLabel ?? (state ? ARIA_BY_STATE[state] : ''));
  const singleDot = $derived(state && state !== 'backgrounded' ? SINGLE_DOT_BY_STATE[state] : null);
</script>

{#if singleDot}
  <span
    class="inline-block h-1.5 w-1.5 shrink-0 rounded-full {singleDot.bg} {singleDot.pulse ? 'animate-pulse' : ''} {className}"
    data-testid="indicator"
    data-state={state}
    role="status"
    aria-label={label}
  ></span>
{:else if state === 'backgrounded'}
  <span
    class="inline-flex shrink-0 items-center gap-[3px] {className}"
    data-testid="indicator"
    data-state="backgrounded"
    role="status"
    aria-label={label}
  >
    {#each BG_DOT_SHIFTS as shiftClass}
      <span class="h-[3.5px] w-[3.5px] rounded-full bg-accent animate-pulse {shiftClass}"></span>
    {/each}
  </span>
{/if}
