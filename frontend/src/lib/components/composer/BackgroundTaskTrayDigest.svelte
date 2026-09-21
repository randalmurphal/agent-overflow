<script lang="ts">
  // The expanded body of an agent's background tray row: the agent's
  // digest (tools, errors, the prompt and its answer) rendered from the
  // SAME rows and row components the inline card and the agent pane use.
  // The tray is a running background agent's live surface, so the digest
  // grows as the agent works; the launch row in the timeline never
  // changes. For the full transcript the row's open button opens the pane.
  //
  // Reads through an agent scope view: the launch's subtree sliced out of
  // the source pane, under a view id of its own so expansion state here
  // never collides with the timeline's or the companion's copy of a row.
  // While mounted the scope is HELD (stores/agentPane.svelte.ts
  // holdAgentScope) so the pane's fold and prune keep its rows loaded.
  import { untrack } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { holdAgentScope } from '../../stores/agentPane.svelte';
  import {
    agentScopeNeedsHydration,
    createAgentScopeView,
  } from '../../stores/agentScopeView.svelte';
  import {
    decoratedSubagentAggregates,
    groupItemsBySubagent,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import { codexSubagentReceiverLabels } from '../../utils/subagentLaunch';
  import { trayTaskLabel, trayTaskScopeId, type TrayTask } from '../../utils/backgroundTray';
  import SubagentDigestBody from '../chat/SubagentDigestBody.svelte';
  import TimelineNodeView from '../chat/TimelineNodeView.svelte';

  let { pane, task, id }: {
    pane: ThreadPane;
    task: TrayTask;
    /** DOM id the row's header points `aria-controls` at. */
    id: string;
  } = $props();

  let scopeId = $derived(trayTaskScopeId(task));
  let threadId = $derived(pane.threadId ?? '');
  let label = $derived(trayTaskLabel(task));

  // One view per scope, disposed with it. The scope id is fixed for a
  // tray row, so in practice this is one view for the digest's lifetime.
  let view = $derived.by(() =>
    createAgentScopeView(pane, scopeId, {
      viewKey: `tray:${scopeId}`,
      openAgentPane: (launchItemId, launchLabel) => pane.openAgentPane(launchItemId, launchLabel),
    }),
  );
  $effect(() => {
    const current = view;
    return () => current.dispose();
  });
  $effect(() => {
    if (!threadId) return;
    const release = holdAgentScope(pane.paneId, threadId, scopeId);
    return () => {
      release();
      // Rows the hold kept outside the loaded window leave with it.
      pane.sweepUnheldAgentScopes();
    };
  });

  let launch = $derived.by(() => {
    void pane.timelineRevision;
    return pane.getItemById(scopeId);
  });

  // A tray row can outlive its launch's place in the loaded window (the
  // window is a tail slice; a long-running agent's launch scrolls above
  // it). One attempt per scope: loadAgentScope brings the row and its
  // children into pane memory under the hold without moving the window
  // or the reader; a miss leaves the honest loading state below.
  let loadAttempted = $state('');
  $effect(() => {
    if (launch || pane.loading) return;
    if (untrack(() => loadAttempted) === scopeId) return;
    loadAttempted = scopeId;
    void pane.loadAgentScope(scopeId);
  });

  let scopedItems = $derived(view.items);
  $effect(() => {
    if (!launch) return;
    const evicted = pane.subagentLiveAggregate(scopeId)?.evictedCount ?? 0;
    if (!agentScopeNeedsHydration(launch, scopedItems.length, evicted)) return;
    void pane.ensureSubagentChildren(scopeId);
  });

  let children = $derived(groupItemsBySubagent(scopedItems, pane.subagentLiveAggregate));
  let receiverLabels = $derived(codexSubagentReceiverLabels(scopedItems));
  let descendantCount = $derived(
    Math.max(launch ? decoratedSubagentAggregates(launch).transcriptCount : 0, scopedItems.length),
  );
  let entryCountLabel = $derived(
    descendantCount === 0 ? '' : `${descendantCount} ${descendantCount === 1 ? 'entry' : 'entries'}`,
  );
  // The latest text is the agent's answer while it runs (its live report)
  // or after a clean completion; a stopped or failed agent's last text is
  // mid-flight prose.
  let keepFinalText = $derived(task.status === 'running' || task.status === 'completed');
</script>

{#snippet renderNode(node: TimelineNode, depth: number)}
  <TimelineNodeView
    pane={view.pane}
    {node}
    {depth}
    codexSubagentReceiverLabels={receiverLabels}
  />
{/snippet}

<div data-testid="background-task-tray-row-digest" data-scope-id={scopeId} aria-label="{label} activity">
  {#if !launch && scopedItems.length === 0}
    <div {id} class="ml-5 border-l border-border-subtle bg-surface-0/35 px-3 py-2" role="region" aria-label="Subagent Timeline">
      <p class="text-xs text-text-secondary italic" data-testid="subagent-group-loading">
        {pane.loading || loadAttempted !== scopeId ? 'Loading agent…' : 'Agent launch is not in the loaded history.'}
      </p>
    </div>
  {:else}
    <SubagentDigestBody
      {id}
      {children}
      {descendantCount}
      {entryCountLabel}
      {keepFinalText}
      live={task.status === 'running'}
      depth={1}
      maxHeight="min(35vh, 14rem)"
      {renderNode}
    />
  {/if}
</div>
