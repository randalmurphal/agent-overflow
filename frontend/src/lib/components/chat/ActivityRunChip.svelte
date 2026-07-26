<script lang="ts">
  // A collapsed activity run: one line standing in for the whole block of
  // tool calls and thinking.
  //
  // Counts alone would not be honest. Collapsing must never hide that
  // something failed or that something is still going, so the chip carries
  // an error dot and a named running indicator alongside the tally — a
  // reader who collapsed a run should still be able to tell, at a glance,
  // whether it needs their attention.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ActivityRunNode } from '../../utils/subagentGrouping';
  import { activityRunSummary } from '../../utils/activityRunSummary';
  import { classifyToolName, TOOL_KIND_COLOR_CLASS } from './toolCardHeader';
  import Indicator from './Indicator.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';

  let {
    pane,
    run,
    clipId,
    onExpand,
  }: {
    pane: ThreadPane;
    run: ActivityRunNode;
    /** The clip this chip expands into, owned by `ActivityRun`. */
    clipId: string;
    onExpand: () => void;
  } = $props();

  // Resolved here, not on the node: counts, failure, and the running label
  // all move on ordinary streaming deltas, and only the chip reads them.
  let items = $derived(
    run.memberItemIds
      .map((id) => pane.getItemById(id))
      .filter((item) => item !== undefined),
  );
  let summary = $derived(activityRunSummary(items));
  // Each term wears its tool's own hue — the same `--ico-*` token the expanded
  // run's icons use, so a chip is recognisable as the block it stands for
  // rather than a grey tally. The count stays muted: the colour identifies
  // WHICH tool, and colouring the number too would just dilute that.
  //
  // Reasoning has no tool name to classify, so it is named by kind. Any tool
  // this build does not know falls out as `generic`, whose token is the
  // ordinary secondary text colour — an unknown tool reads as plain text
  // instead of borrowing a hue that means something else.
  let terms = $derived(
    summary.counts.entries.map((entry) => ({
      ...entry,
      key: `${entry.isThinking ? 'T' : 'C'}:${entry.label}`,
      colorClass: TOOL_KIND_COLOR_CLASS[
        entry.isThinking ? 'brain' : classifyToolName(entry.label).icon
      ],
    })),
  );
  let ariaLabel = $derived(
    `Expand ${summary.counts.total} activity ${summary.counts.total === 1 ? 'row' : 'rows'}`,
  );
</script>

<TranscriptDisclosureHeader
  expanded={false}
  testId="activity-run-chip"
  {ariaLabel}
  controls={clipId}
  onToggle={onExpand}
  class="py-0.5"
>
  {#snippet children()}
    <!-- One text run, no gaps: the separators are literal text so the line
         truncates mid-term like the plain string it replaced. -->
    <span class="min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted" data-testid="activity-run-chip-counts">
      {#each terms as term, i (term.key)}{i > 0 ? ', ' : ''}{term.count}{' '}<span
          class={term.colorClass}
          data-tool-term={term.label}
        >{term.label}</span>{/each}
    </span>
    {#if summary.hasFailure}
      <span class="flex shrink-0 items-center" data-testid="activity-run-chip-failure">
        <Indicator state="error" ariaLabel="Contains a failure" />
      </span>
    {/if}
    {#if summary.runningLabel}
      <span
        class="flex shrink-0 items-center gap-1.5 text-[0.6875rem] text-fg-hint"
        data-testid="activity-run-chip-running"
      >
        <Indicator state="running" ariaLabel="Running {summary.runningLabel}" />
        {summary.runningLabel}
      </span>
    {/if}
  {/snippet}
</TranscriptDisclosureHeader>
