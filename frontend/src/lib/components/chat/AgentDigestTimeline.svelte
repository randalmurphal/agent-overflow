<script lang="ts">
  import { untrack } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { createAgentScopeView, type AgentScopeView } from '../../stores/agentScopeView.svelte';
  import { collectTimelineNodeItemIds, groupItemsBySubagent, timelineNodeKey, type TimelineNode } from '../../utils/subagentGrouping';
  import { ACTIVITY_RUN_CHUNK_ROWS } from '../../utils/activityRunWindow';
  import { groupActivityRuns } from '../../utils/activityRunGrouping';
  import { codexSubagentReceiverLabels } from '../../utils/subagentLaunch';
  import { subagentDigestNodes } from '../../utils/subagentDigest';
  import SubagentBodyClip from './SubagentBodyClip.svelte';
  import TimelineNodeView from './TimelineNodeView.svelte';

  let { pane, scopeId, id, viewKey, digestItemId, keepFinalText = true, live, maxHeight }: {
    pane: ThreadPane; scopeId: string; id: string; viewKey: string; digestItemId?: string;
    keepFinalText?: boolean; live: boolean; maxHeight?: string;
  } = $props();
  let sourceThreadId = $derived(pane.threadId);
  let view = $state.raw<AgentScopeView | null>(null);
  $effect(() => {
    const source = pane;
    const scope = scopeId;
    const rowId = viewKey;
    const execution = digestItemId;
    if (!sourceThreadId) return;
    const current = untrack(() => createAgentScopeView(source, scope, {
      viewKey: rowId, toolsOnly: !execution, digestItemId: execution,
      openAgentPane: (id, label) => source.openAgentPane(id, label),
    }));
    view = current;
    fetching = new Set();
    untrack(() => current.start());
    return () => { current.dispose(); if (view === current) view = null; };
  });
  type Boundary = { kind: 'boundary'; id: string; runId?: string; direction: 'before' | 'after'; count?: number };
  type DigestNode = TimelineNode | Boundary;
  let fetching = $state<Set<string>>(new Set());
  let nodes = $derived.by<DigestNode[]>(() => {
    if (!view) return [];
    const scoped = view.pane;
    scoped.activityRuns.revision;
    scoped.loading;
    const items = scoped.items;
    const groups = untrack(() => groupActivityRuns(groupItemsBySubagent(items), {
      identity: scoped.activityRuns, getItem: scoped.getItemById, withheld: [],
      windowReachesTail: !scoped.hasMoreNewer,
    }));
    const result: DigestNode[] = [];
    if (scoped.hasMoreHistory) result.push({ kind: 'boundary', id: 'older', direction: 'before' });
    for (const node of groups) {
      if (node.kind !== 'activity_run') { result.push(...(digestItemId ? subagentDigestNodes([node], keepFinalText) : [node])); continue; }
      // A boundary belongs to its loaded edge. When that edge moves, the
      // virtualizer anchors the surviving tool row through the prepend.
      if (node.unshippedBefore) result.push({ kind: 'boundary', id: `${node.runId}:before:${node.loadedFirstItemId}`, runId: node.runId, direction: 'before', count: node.unshippedBefore });
      result.push(...(digestItemId ? subagentDigestNodes(node.children, keepFinalText) : node.children));
      if (node.unshippedAfter) result.push({ kind: 'boundary', id: `${node.runId}:after:${node.loadedLastItemId}`, runId: node.runId, direction: 'after', count: node.unshippedAfter });
    }
    if (scoped.hasMoreNewer) result.push({ kind: 'boundary', id: 'newer', direction: 'after' });
    return result;
  });
  async function loadBoundary(boundary: Boundary) {
    const current = view;
    if (!current || fetching.has(boundary.id)) return;
    fetching = new Set(fetching).add(boundary.id);
    try {
      if (boundary.runId) await current.pane.activityRuns.fetchMembers(boundary.runId, {
        direction: boundary.direction, limit: ACTIVITY_RUN_CHUNK_ROWS,
      });
      else if (boundary.direction === 'before') await current.pane.loadOlder();
      else await current.pane.loadNewer();
    } finally {
      if (view === current) fetching = new Set([...fetching].filter(id => id !== boundary.id));
    }
  }
  // A boundary loads when it reaches the clip viewport, independently of the
  // virtualizer's overscan. Each run retains its own membership cursor.
  function observeBoundary(element: HTMLElement, getBoundary: () => Boundary) {
    const observer = new IntersectionObserver(entries => {
      if (entries.some(entry => entry.isIntersecting)) void loadBoundary(getBoundary());
    }, { root: element.closest('[data-testid="subagent-group-scroll"]') });
    observer.observe(element);
    return { destroy() { observer.disconnect(); } };
  }
  let receiverLabels = $derived(codexSubagentReceiverLabels(view?.items ?? []));
  let windowOwner = $derived(view ? {
    attach: view.pane.attachScrollController,
    detach: view.pane.detachScrollController,
    itemIds(node: DigestNode): Iterable<string> {
      const ids = new Set<string>();
      if (node.kind !== 'boundary') collectTimelineNodeItemIds(node, ids);
      return ids;
    },
  } : undefined);
</script>

{#snippet renderNode(node: DigestNode, depth: number)}
  {#if node.kind === 'boundary'}
    <button class="block w-full py-1 text-left text-xs text-accent" use:observeBoundary={() => node}
      disabled={fetching.has(node.id)} onclick={() => loadBoundary(node)}>
      {fetching.has(node.id) ? 'Loading…' : `${node.count ?? 'Load'} ${node.direction === 'before' ? 'earlier' : 'newer'} activities`}
    </button>
  {:else if view}
    <TimelineNodeView pane={view.pane} {node} {depth} codexSubagentReceiverLabels={receiverLabels} />
  {/if}
{/snippet}

<div {id} class="ml-5 border-l border-border-subtle bg-surface-0/35 px-3 py-2" role="region" aria-label="Subagent Timeline" data-testid="subagent-group-body">
    {#if view?.gone}
      <p role="status" class="text-xs text-text-secondary">Agent activity is no longer available.</p>
    {:else if view?.error}
      <p role="alert" class="text-xs text-text-secondary">{view.error}</p>
      <button class="text-xs text-accent" onclick={() => view?.pane.retryHistoryLoad()}>Retry</button>
    {/if}
    {#if !view?.gone && nodes.length > 0}
      <SubagentBodyClip getKey={node => node.kind === 'boundary' ? `boundary:${node.id}` : timelineNodeKey(node)} {nodes} depth={1} {live} {maxHeight} {renderNode} {windowOwner} />
    {:else if !view?.gone && !view?.error}
      <p class="text-xs text-text-secondary italic">{view?.pane.loading ? 'Loading activity…' : digestItemId ? 'No activity yet.' : 'No tool activity yet.'}</p>
    {/if}
</div>
