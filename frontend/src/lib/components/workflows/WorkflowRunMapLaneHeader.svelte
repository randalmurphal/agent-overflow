<script lang="ts">
  // A fan lane's header line (RUN-MAP §6/§7): glyph or spinner, the lane's
  // title, its duration, and — on a lane that owns a fold — the "[N runs]"
  // disclosure. This is the whole render of a FOLDED lane, which is why it is
  // a component and not markup inside the fan: both fan layouts draw the same
  // line, and the folded lane's one-rigid-line rule lives in exactly one
  // place.

  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { truncateMiddle } from '../../utils/format';
  import type { RunMapBranch } from '../../utils/workflowRunMap';
  import {
    runMapNodeStyle,
    RUN_MAP_FOLDED_LABEL_MAX,
    RUN_MAP_LABEL_MAX,
    RUN_MAP_LANE_HEADER,
  } from '../../utils/workflowRunMapStyle';

  interface Props {
    lane: RunMapBranch;
    /** True inside a `stacked` fan, where the header left-aligns with its block. */
    stacked: boolean;
    nowKey: string;
    onOpenThread: (threadId: string) => void;
    onToggleLane: (branchKey: string) => void;
  }
  let { lane, stacked, nowKey, onOpenThread, onToggleLane }: Props = $props();

  let chip = $derived(lane.unit);
  let style = $derived(runMapNodeStyle(chip.signal));
  let isNow = $derived(chip.key === nowKey);
  let folded = $derived(lane.collapsed);
</script>

<div
  class={[
    'flex items-baseline gap-1.5',
    stacked ? 'justify-start' : 'justify-center',
    folded ? 'whitespace-nowrap' : 'flex-wrap',
    style.tone,
  ].filter(Boolean).join(' ')}
  data-run-map-now={isNow ? 'true' : undefined}
  data-unit-id={chip.unitId}
  data-unit-signal={chip.signal}
>
  {#if isNow}
    <span class="shrink-0 text-[0.625rem] font-semibold tracking-wider text-accent">now ▸</span>
  {/if}
  {#if style.spinner}
    <SteppedSpinner size={9} class="shrink-0 self-center" animate={!getSettings().lowPowerMode} />
  {:else}
    <span class={['shrink-0 text-[0.6875rem]', style.glyphTone].join(' ')} aria-hidden="true">{style.glyph}</span>
  {/if}
  <button
    type="button"
    class={[
      RUN_MAP_LANE_HEADER,
      'text-left hover:underline disabled:cursor-default disabled:no-underline',
      // The title never ellipsizes in CSS — it is the lane's identity in BOTH
      // states. An OPEN lane wraps under the runaway guard, because a header
      // that reads `POR…` beside a fully-named neighbour says nothing at all;
      // a FOLDED lane is the map's one deliberately rigid line, so it takes
      // the hard summary budget instead (§6, text).
      folded ? 'whitespace-nowrap' : 'min-w-0 break-words',
      chip.struck ? 'line-through' : '',
    ].filter(Boolean).join(' ')}
    disabled={!chip.threadId}
    title={lane.title}
    onclick={() => onOpenThread(chip.threadId)}
    data-testid="workflow-map-lane-name"
  >
    {truncateMiddle(lane.title, folded ? RUN_MAP_FOLDED_LABEL_MAX : RUN_MAP_LABEL_MAX)}
  </button>
  {#if chip.duration}
    <span class="shrink-0 text-[0.625rem] tabular-nums text-fg-hint">{chip.duration}</span>
  {/if}
  {#if lane.toggleable}
    <!--
      A settled lane's subtree is one click away, and the click says how much
      is behind it rather than being a bare chevron. Inline with the name, so
      the lane really is ONE line while it is folded (§7).
    -->
    <button
      type="button"
      class="shrink-0 rounded-full bg-surface-2/60 px-1.5 text-[0.625rem] text-fg-muted hover:bg-surface-2"
      aria-expanded={!lane.collapsed}
      onclick={() => onToggleLane(lane.key)}
      data-testid="workflow-map-lane-toggle"
    >
      {chip.childRunCount === 1 ? '1 run' : `${chip.childRunCount} runs`}
    </button>
  {/if}
</div>
