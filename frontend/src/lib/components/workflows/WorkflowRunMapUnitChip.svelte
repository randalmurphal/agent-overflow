<script lang="ts">
  // One fan unit drawn as a NODE (RUN-MAP §2, §6): the join under the rejoin
  // bar, and every entry inside the done group's expansion.
  //
  // Its own component rather than a snippet in the fan because it is the same
  // statement in two structurally unrelated places — a chip in a wrapping grid
  // and the single node the spine rejoins into — and because a lane HEADER is
  // deliberately not this: a header is a borderless summary line on top of a
  // column, a chip is a box that stands for the unit itself.

  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { truncateMiddle } from '../../utils/format';
  import type { RunMapUnitChip } from '../../utils/workflowRunMap';
  import {
    runMapNodeStyle,
    RUN_MAP_LABEL_MAX,
    RUN_MAP_NODE_BOX,
  } from '../../utils/workflowRunMapStyle';

  interface Props {
    chip: RunMapUnitChip;
    /** The follow target's key — the one element that carries the `now ▸` tag. */
    nowKey: string;
    onOpenThread: (threadId: string) => void;
  }
  let { chip, nowKey, onOpenThread }: Props = $props();

  let style = $derived(runMapNodeStyle(chip.signal));
  let isNow = $derived(chip.key === nowKey);
</script>

<div
  class={[RUN_MAP_NODE_BOX, 'bg-surface-1/30', style.border, style.glow].filter(Boolean).join(' ')}
  data-run-map-now={isNow ? 'true' : undefined}
  data-unit-id={chip.unitId}
  data-unit-signal={chip.signal}
>
  {#if isNow}
    <span class="shrink-0 text-[0.625rem] font-semibold tracking-wider text-accent">now ▸</span>
  {/if}
  {#if style.spinner}
    <SteppedSpinner size={10} class="shrink-0 self-center" animate={!getSettings().lowPowerMode} />
  {:else}
    <span class={['shrink-0 text-[0.6875rem]', style.tone].join(' ')} aria-hidden="true">{style.glyph}</span>
  {/if}
  <button
    type="button"
    class={[
      'min-w-0 truncate text-left text-[0.6875rem] hover:underline disabled:cursor-default disabled:no-underline',
      style.label,
      chip.struck ? 'line-through' : '',
    ].filter(Boolean).join(' ')}
    disabled={!chip.threadId}
    title={chip.label}
    onclick={() => onOpenThread(chip.threadId)}
  >
    {truncateMiddle(chip.label, RUN_MAP_LABEL_MAX)}
  </button>
  <span class="shrink-0 text-[0.625rem] tabular-nums text-fg-hint">{chip.meta}</span>
</div>
