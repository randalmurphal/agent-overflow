<script lang="ts">
  // A wave's row (RUN-MAP §3, §6): `✓ wave N · 2h 30m · outcome · units`.
  //
  // It is the wave's header in BOTH states, not only the folded one. A folded
  // wave and an expanded wave then differ by what hangs beneath the same row
  // rather than by which component drew it, which is what lets the fold
  // animate (§10) instead of swapping one row for another — and what keeps a
  // live wave from carrying two headers saying the same thing.
  //
  // It takes the ROW's parts rather than a `RunMapWave`, because a lap inside
  // an open composition folds by exactly the same rule and has to look exactly
  // the same (§3). A component that took the whole wave could not serve both
  // without one of them growing a second renderer that starts identical.
  //
  // One flex line, and the parts yield by CSS priority alone (outcome > unit
  // counts > duration) through shrink order and `min-width: 0` — no JS
  // measurement anywhere near the map.

  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import type { RunMapSignal, RunMapWaveSummary } from '../../utils/workflowRunMap';
  import { runMapNodeStyle } from '../../utils/workflowRunMapStyle';

  interface Props {
    summary: RunMapWaveSummary;
    signal: RunMapSignal;
    ordinal: number;
    expanded: boolean;
    /** A live wave is expanded in place and has nothing to fold back into. */
    toggleable: boolean;
    onToggle: () => void;
  }
  let { summary, signal, ordinal, expanded, toggleable, onToggle }: Props = $props();

  let style = $derived(runMapNodeStyle(signal));
  let outcome = $derived([summary.outcomeLabel, summary.reasonLabel]
    .filter((part) => part !== '')
    .join(' · '));
</script>

<button
  type="button"
  class={[
    'flex w-full items-baseline gap-2 rounded-md px-2 py-1 text-left',
    toggleable ? 'hover:bg-surface-2/50' : 'cursor-default',
    style.glow,
  ].filter(Boolean).join(' ')}
  disabled={!toggleable}
  aria-expanded={toggleable ? expanded : undefined}
  onclick={onToggle}
  data-testid="workflow-map-summary"
  data-wave-ordinal={ordinal}
  data-signal={signal}
>
  {#if style.spinner}
    <SteppedSpinner size={11} class="shrink-0 self-center" animate={!getSettings().lowPowerMode} />
  {:else}
    <span class={['shrink-0 text-xs', style.tone].join(' ')} aria-hidden="true">{style.glyph}</span>
  {/if}

  <span class={['shrink-0 text-xs', style.label].join(' ')}>{summary.label}</span>

  {#if summary.duration}
    <span class="min-w-0 shrink-[3] truncate text-[0.6875rem] tabular-nums text-fg-hint">{summary.duration}</span>
  {/if}

  {#if outcome}
    <span class={['shrink-0 text-[0.6875rem]', style.tone].join(' ')}>{outcome}</span>
  {/if}

  {#if summary.unitsLabel}
    <span class="min-w-0 shrink truncate text-[0.6875rem] text-fg-muted">{summary.unitsLabel}</span>
  {/if}

  {#if summary.retriesLabel}
    <span class="min-w-0 shrink-[2] truncate text-[0.6875rem] text-fg-muted">{summary.retriesLabel}</span>
  {/if}
</button>
