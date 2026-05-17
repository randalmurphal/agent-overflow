<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';
  import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';
  import RowError from './RowError.svelte';
  import type { RowErrorData } from './rowState';

  let {
    pane,
    itemId,
    promptPreview,
    rowError,
    completionPreview,
    expanded,
    tool,
    isCompletion,
    receivers,
    receiverDisplayLabels,
    statusLine,
    expansion,
  }: {
    pane?: ThreadPane;
    itemId: string;
    promptPreview: string;
    rowError: RowErrorData | null;
    completionPreview: string;
    expanded: boolean;
    tool: string;
    isCompletion: boolean;
    receivers: string[];
    receiverDisplayLabels: string[];
    statusLine: (id: string) => string;
    expansion: PayloadExpansionHandle | null;
  } = $props();
</script>

{#if promptPreview}
  <div class="ml-5 mt-0.5 truncate text-[11px] text-fg-subtle">└ {promptPreview}</div>
{/if}
{#if rowError}
  <div class="ml-[5.25rem] px-3 pb-1">
    <RowError tone={rowError.tone} msg={rowError.msg} />
  </div>
{/if}
{#if completionPreview && !expanded}
  <div class="ml-5 mt-0.5 truncate text-[11px] text-fg-subtle" data-testid="collab-tool-row-preview">
    └ {completionPreview}
  </div>
{/if}
{#if tool === 'wait_agent' && receivers.length > 0 && (isCompletion || receiverDisplayLabels.length > 1)}
  <!--
    `ml-[6.125rem]` lines the receiver list up with the parent row's
    body column (where "Waiting for N agents" starts), so each agent
    being waited on reads as a continuation of the parent's body
    rather than a separate left-edge list. Math from the disclosure
    primitive's gutter widths (see TranscriptDisclosureHeader):
      chevron size-3 (0.75rem) + gap-2 (0.5rem)
      + icon size-3.5 (0.875rem) + gap-2 (0.5rem)
      + label w-12 (3rem) + gap-2 (0.5rem)
      = 6.125rem inside the CollabToolRow's `px-1` content edge.
    No `└` prefix on the receiver rows — the body-column alignment
    is the visual cue that these belong to the wait header above;
    a leader glyph on top of that would double up.
  -->
  <div
    class="ml-[6.125rem] mt-0.5 space-y-0.5 text-[11px] text-fg-subtle"
    data-testid="collab-tool-row-receivers"
  >
    {#each receivers as id, index}
      <div class="truncate">
        {isCompletion ? statusLine(id) : receiverDisplayLabels[index]}
      </div>
    {/each}
  </div>
{/if}
{#if expansion && expanded}
  <ExpandablePayloadBody
    {pane}
    {expansion}
    id="collab-tool-row-output-{itemId}"
    testPrefix="collab-tool-row"
    bodyTestId="collab-tool-row-output"
    outputTestId="collab-tool-row-output-text"
    emptyMessage="No stored output for this agent."
  />
{/if}
