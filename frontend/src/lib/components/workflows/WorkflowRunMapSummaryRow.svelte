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
  // One WRAPPING flex row: every part renders in full, and a narrow card gets
  // a second line rather than a hidden count — CSS ellipsis is banned on this
  // surface (§2), and each part is short and atomic (`whitespace-nowrap`) so
  // the wrap point falls between facts, never inside one. No JS measurement
  // anywhere near the map.

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
    'flex w-full flex-wrap items-baseline gap-x-2 gap-y-0.5 rounded-md px-2 py-1 text-left',
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
    <span class={['shrink-0 text-xs', style.glyphTone].join(' ')} aria-hidden="true">{style.glyph}</span>
  {/if}

  <span class={['shrink-0 whitespace-nowrap text-xs', style.label].join(' ')}>{summary.label}</span>

  {#if summary.duration}
    <span class="whitespace-nowrap text-[0.6875rem] tabular-nums text-fg-hint">{summary.duration}</span>
  {/if}

  {#if outcome}
    <!-- The one part that can be a sentence (a reason label), so it wraps
         INTERNALLY rather than forcing the row wide. -->
    <span class={['min-w-0 break-words text-[0.6875rem]', style.tone].join(' ')}>{outcome}</span>
  {/if}

  {#if summary.unitsLabel}
    <span class="whitespace-nowrap text-[0.6875rem] text-fg-muted">{summary.unitsLabel}</span>
  {/if}

  {#if summary.retriesLabel}
    <span class="whitespace-nowrap text-[0.6875rem] text-fg-muted">{summary.retriesLabel}</span>
  {/if}
</button>
