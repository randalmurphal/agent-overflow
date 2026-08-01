<script lang="ts">
  // An activity run's header: one line summarising the whole block of tool
  // calls and thinking, and the control that opens and closes it.
  //
  // Present in BOTH states, which is the point. It used to render only while
  // collapsed, so expanding a run made the thing the reader had just clicked
  // disappear and left the invisible rail strip as the only way back — the
  // worst feedback a disclosure control can give. Always-on also means the
  // run has a visible boundary, and that the fold has something that does not
  // move while the clip closes underneath it.
  //
  // Counts alone would not be honest, and that holds while expanded too: an
  // open run mounts only `activityRunWindowRows` of its rows, so a failure or
  // a running tool can sit outside the window exactly the way a collapsed run
  // hides one. The error dot and the named running indicator ride along in
  // both states for the same reason.

  import { untrack } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ActivityRunNode } from '../../utils/subagentGrouping';
  import { activityRunSummary } from './activityRunSummary';
  import { TOOL_KIND_COLOR_CLASS } from './toolCardHeader';
  import Indicator from './Indicator.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';

  let {
    pane,
    run,
    clipId,
    expanded,
    onToggle,
    class: className = '',
  }: {
    pane: ThreadPane;
    run: ActivityRunNode;
    /** The clip this header opens and closes, owned by `ActivityRun`. */
    clipId: string;
    expanded: boolean;
    onToggle: () => void;
    /** Extra classes from `ActivityRun` — the chevron-on-rail alignment
     * lives there, next to the rail offsets it must stay in sync with. */
    class?: string;
  } = $props();

  // Resolved here, not on the node: counts, failure, and the running label
  // all move on ordinary streaming deltas, and only this header reads them.
  //
  // Resolved through a signature cutoff, though: while a member row
  // streams, the per-item smoother replaces its item object on every
  // reveal tick (~50Hz), which invalidates any derived that reads the
  // member items — but the summary's output depends only on the fields
  // below, none of which change on a reveal tick. The signature derived
  // stays tracked (it is what re-runs on every tick and on every real
  // transition); string equality then cuts propagation, so the heavy
  // summary work (Set of ids, presentation map, count buckets, sort)
  // re-runs only when a member's identity/status/kind/tool actually
  // changes. `completionOf` is in the key because the orphan-completion
  // dedup reads it. The summary body runs untracked so its item reads
  // don't re-subscribe it to per-tick item replacement.
  let summaryKey = $derived.by(() => {
    let key = pane.thread?.provider ?? '';
    for (const id of run.memberItemIds) {
      const item = pane.getItemById(id);
      if (!item) continue;
      key += `\u0000${item.id}\u0001${item.kind}\u0001${item.status}\u0001${item.toolName ?? ''}\u0001${item.completionOf ?? ''}`;
    }
    return key;
  });
  let summary = $derived.by(() => {
    void summaryKey;
    return untrack(() => {
      const items = run.memberItemIds
        .map((id) => pane.getItemById(id))
        .filter((item) => item !== undefined);
      return activityRunSummary(items, pane.thread?.provider);
    });
  });
  // Each term wears its tool's own hue — the same `--ico-*` token the run's
  // own icons use, so the summary is recognisable as the block it describes
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
      colorClass: TOOL_KIND_COLOR_CLASS[entry.icon],
    })),
  );
  let ariaLabel = $derived(
    `${expanded ? 'Collapse' : 'Expand'} ${summary.counts.total} activity `
      + `${summary.counts.total === 1 ? 'row' : 'rows'}`,
  );
</script>

<TranscriptDisclosureHeader
  {expanded}
  testId="activity-run-header"
  {ariaLabel}
  controls={clipId}
  {onToggle}
  class={['py-0.5', className].filter(Boolean).join(' ')}
>
  {#snippet children()}
    <!-- One text run, no gaps: the separators are literal text so the line
         truncates mid-term like the plain string it replaced. -->
    <span class="min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted" data-testid="activity-run-header-counts">
      {#each terms as term, i (term.key)}{i > 0 ? ', ' : ''}{term.count}{' '}<span
          class={term.colorClass}
          data-tool-term={term.label}
        >{term.label}</span>{/each}
    </span>
    {#if summary.hasFailure}
      <span class="flex shrink-0 items-center" data-testid="activity-run-header-failure">
        <Indicator state="error" ariaLabel="Contains a failure" />
      </span>
    {/if}
    {#if summary.runningLabel}
      <span
        class="flex shrink-0 items-center gap-1.5 text-[0.6875rem] text-fg-hint"
        data-testid="activity-run-header-running"
      >
        <Indicator state="running" ariaLabel="Running {summary.runningLabel}" />
        {summary.runningLabel}
      </span>
    {/if}
  {/snippet}
</TranscriptDisclosureHeader>
