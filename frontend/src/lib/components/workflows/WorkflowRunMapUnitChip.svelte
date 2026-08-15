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
  class={[
    RUN_MAP_NODE_BOX,
    style.border,
    style.glow,
    isNow ? 'bg-accent/10' : style.fill,
  ].filter(Boolean).join(' ')}
  data-run-map-now={isNow ? 'true' : undefined}
  data-unit-id={chip.unitId}
  data-unit-signal={chip.signal}
>
  <!-- ONE button holding glyph, label and meta as inline content, same shape
       as the node row: separate atomic siblings wrap as UNITS when space runs
       out, stranding a lone glyph on the first line. The glyph and meta are
       `inline-block` so the hover underline stays on the words. -->
  <button
    type="button"
    class="max-w-full break-words text-left hover:underline disabled:cursor-default disabled:no-underline"
    disabled={!chip.threadId}
    title={chip.label}
    onclick={() => onOpenThread(chip.threadId)}
  >
    {#if isNow}
      <span class="mr-1 inline-block text-[0.625rem] font-semibold tracking-wider text-accent">now ▸</span>
    {/if}
    {#if style.spinner}
      <SteppedSpinner size={10} class="mr-1 inline-block align-middle" animate={!getSettings().lowPowerMode} />
    {:else}
      <span class={['mr-1 inline-block text-[0.6875rem]', style.glyphTone].join(' ')} aria-hidden="true">{style.glyph}</span>
    {/if}
    <!-- Wraps, never ellipsizes (§2): the unit id is the chip's whole meaning. -->
    <span
      class={['text-[0.6875rem]', style.label, chip.struck ? 'line-through' : ''].filter(Boolean).join(' ')}
    >{truncateMiddle(chip.label, RUN_MAP_LABEL_MAX)}</span>
    <span class="ml-1 inline-block text-[0.625rem] tabular-nums text-fg-hint">{chip.meta}</span>
  </button>
</div>
