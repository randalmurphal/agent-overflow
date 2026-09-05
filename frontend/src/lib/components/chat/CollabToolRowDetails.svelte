<script lang="ts">
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';
  import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';
  import RowError from './RowError.svelte';
  import type { RowErrorData } from './rowState';

  let {
    pane,
    bodyDomId,
    promptPreview,
    rowError,
    completionPreview,
    expanded,
    tool,
    receiverDisplayLabels,
    expansion,
    emptyMessage,
  }: {
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    /** Owned by `CollabToolRow`, which also states it as the header's
     * `controls` — see utils/chatDomIds.ts. */
    bodyDomId: string;
    promptPreview: string;
    rowError: RowErrorData | null;
    completionPreview: string;
    expanded: boolean;
    tool: string;
    receiverDisplayLabels: string[];
    expansion: PayloadExpansionHandle | null;
    /** Body copy when there is no payload. Owned by `CollabToolRow`, which
     * holds the item this row's output belongs to and so is the one that can
     * tell a missing payload from one an import could not carry over. */
    emptyMessage: string;
  } = $props();
</script>

{#if promptPreview}
  <div class="ml-5 mt-0.5 truncate text-[0.6875rem] text-fg-subtle">└ {promptPreview}</div>
{/if}
{#if rowError}
  <div class="ml-[5.25rem] compact:ml-5 px-3 pb-1">
    <RowError tone={rowError.tone} msg={rowError.msg} />
  </div>
{/if}
{#if completionPreview && !expanded}
  <div class="ml-5 mt-0.5 truncate text-[0.6875rem] text-fg-subtle" data-testid="collab-tool-row-preview">
    └ {completionPreview}
  </div>
{/if}
{#if tool === 'wait_agent' && receiverDisplayLabels.length > 0}
  <!--
    Match prompt and completion previews so the wait targets read as
    secondary detail for both active and finished waits. A comma-separated
    line keeps long rosters compact without one row per agent.
    Labels only, on the carrier and the finished header alike: each
    agent's output already lives on its spawn completion row beneath
    the wait group, so repeating per-agent statuses or final messages
    here would duplicate it into an unbounded header line.
  -->
  <div
    class="ml-5 mt-0.5 truncate text-[0.6875rem] text-fg-subtle"
    data-testid="collab-tool-row-receivers"
  >
    └ {receiverDisplayLabels.join(', ')}
  </div>
{/if}
{#if expansion && expanded}
  <ExpandablePayloadBody
    {pane}
    {expansion}
    id={bodyDomId}
    testPrefix="collab-tool-row"
    bodyTestId="collab-tool-row-output"
    outputTestId="collab-tool-row-output-text"
    {emptyMessage}
  />
{/if}
