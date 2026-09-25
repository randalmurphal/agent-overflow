<script lang="ts">
  // An agent card's expanded body: a parked stop's full report above the
  // digest of the run the card shows. The digest is the pane's scoped
  // window, bounded to the card's stop, or the node's own children where
  // no pane exists.
  import type { Snippet } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import type { SubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';
  import { agentScopeRootId } from '../../utils/subagentLaunch';
  import AgentDigestTimeline from './AgentDigestTimeline.svelte';
  import SubagentDigestBody from './SubagentDigestBody.svelte';
  import SubagentParkedReport from './SubagentParkedReport.svelte';

  let {
    pane,
    group,
    parent,
    completionItem,
    id,
    reportItemId,
    descendantCount,
    entryCountLabel,
    keepFinalText,
    live,
    depth,
    renderNode,
  }: {
    pane?: ThreadPane;
    group: SubagentGroupNode;
    parent: Item;
    completionItem: Item | null;
    /** DOM id the card's disclosure header points `aria-controls` at. */
    id: string;
    /** A parked stop's report row, shown above the digest; '' for none. */
    reportItemId: string;
    descendantCount: number;
    entryCountLabel: string;
    keepFinalText: boolean;
    live: boolean;
    /** Depth of the card; the digest rows render one deeper. */
    depth: number;
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();
</script>

{#if reportItemId}
  <SubagentParkedReport threadId={parent.threadId} {reportItemId} />
{/if}
{#if pane}
  <AgentDigestTimeline {pane} scopeId={agentScopeRootId(parent)} {id}
    viewKey={`card:${group.groupKey}`} digestItemId={completionItem?.id ?? parent.id}
    {keepFinalText} {live} />
{:else}
  <SubagentDigestBody
    {id}
    children={group.children}
    {descendantCount}
    {entryCountLabel}
    {keepFinalText}
    {live}
    depth={depth + 1}
    {renderNode}
  />
{/if}
