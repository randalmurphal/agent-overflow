<script lang="ts">
  // The frontier strip (RUN-MAP §8): where the run IS, what blocks it, when it
  // resumes, and what it has spent — one line above the spine.
  //
  // It sits at the top of the MAP rather than in the run header, which keeps
  // its existing role (§8). Everything here is a read: the breadcrumb, the
  // blocker's reason word and the money summary are all precomputed by the
  // projection, and the one thing that ticks — the resume countdown — is
  // formatted from the shared clock the map threads in, because a countdown is
  // the only value on this strip whose answer changes without an event.

  import { runMapNodeStyle } from '../../utils/workflowRunMapStyle';
  import { formatCountdownSpan } from '../../utils/format';
  import type { RunMapFrontierEntry, RunMapLoop } from '../../utils/workflowRunMap';

  interface Props {
    /** The follow target — the leaf the run is at, or null when it has none. */
    target: RunMapFrontierEntry | null;
    loop: RunMapLoop | null;
    /** `model.moneyLabel`: the ONE money string, never re-derived here (§11). */
    money: string;
    /**
     * `model.budgetLabel`: a token or wall-clock ceiling in its own units, and
     * empty for a dollar one, which `money` already compares. Both are
     * precomputed for the same reason every other string here is.
     */
    budget: string;
    /** The shared 1Hz clock, for the resume countdown alone. */
    nowMs: number;
  }
  let { target, loop, money, budget, nowMs }: Props = $props();

  let resumeSpan = $derived(target !== null && target.autoResumeAt > 0
    ? formatCountdownSpan(target.autoResumeAt, nowMs)
    : '');
</script>

<div
  class="mb-2 flex flex-wrap items-baseline gap-x-2 gap-y-1 border-b border-border-subtle pb-2"
  data-testid="workflow-map-frontier"
>
  {#if target !== null}
    <nav class="min-w-0 flex-1 truncate text-[0.6875rem] text-fg-muted" aria-label="Frontier">
      {#each target.path as part, index (part.key)}
        {#if index > 0}<span class="px-1 text-fg-subtle">›</span>{/if}<span
          class={index === target.path.length - 1 ? 'text-fg' : ''}>{part.label}</span>
      {/each}
    </nav>

    {#if target.needsHuman}
      <!--
        The blocker chip is the frontier strip's one hue, and it is R1's
        amber for human-blocked — declared where every other amber on the
        surface is, so the strip and the node it points at cannot drift.
      -->
      {@const blocker = runMapNodeStyle('parked')}
      <span
        class={['shrink-0 rounded border px-1.5 py-0.5 text-[0.6875rem]',
          blocker.border, blocker.glow, blocker.tone].join(' ')}
        data-testid="workflow-map-blocker"
      >
        {target.reasonLabel || 'Needs you'}
      </span>
    {/if}

    {#if resumeSpan}
      <span class="shrink-0 text-[0.6875rem] tabular-nums text-fg-muted" data-testid="workflow-map-resume">
        {resumeSpan === 'now' ? 'resuming now' : `resumes in ${resumeSpan}`}
      </span>
    {/if}
  {/if}

  {#if loop !== null || money !== '' || budget !== ''}
    <span class="shrink-0 text-[0.6875rem] tabular-nums text-fg-hint" data-testid="workflow-map-lap">
      {[loop?.lapLabel ?? '', money, budget, loop?.softStopNote ?? '']
        .filter((part) => part !== '').join(' · ')}
    </span>
  {/if}
</div>
