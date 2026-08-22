<script lang="ts">
  // The `agent` companion pane (docs/specs/agent-visibility.md Q4/Q5): a
  // READ-ONLY view of the source thread's transcript scoped to one
  // subagent launch. Unlike the inline card's digest body, this surface
  // shows everything the node produced — tool calls, thinking,
  // intermediate text, nested child cards — rendered through the same row
  // components the chat timeline uses, so nothing here can drift from
  // transcript styling.
  //
  // Scope changes swap in place (one pane per source pane, no stacking):
  // descending into a child card grows the breadcrumb (ctx.openAgentScope
  // → pushScope), a breadcrumb click pops back, and popping to the root
  // leaves the empty scope, which this body answers by closing the pane.
  //
  // The rows receive the REAL source ThreadPane — expansion registries,
  // live resolution, approvals all stay shared with the chat surface —
  // wrapped in a proxy that overrides only `paneId`. chatDomIds scopes
  // every disclosure id by paneId, and the same item can be mounted here
  // and in the chat timeline at once; without the override the two copies
  // would emit duplicate DOM ids and aria-controls would resolve to
  // whichever came first.
  import { untrack } from 'svelte';
  import X from '@lucide/svelte/icons/x';
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import { agentStateForPane } from '../../stores/agentPane.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Item } from '../../types/models';
  import TimelineLeaf from '../chat/TimelineLeaf.svelte';
  import SubagentGroup from '../chat/SubagentGroup.svelte';
  import WaitGroup from '../chat/WaitGroup.svelte';
  import ReadGroupRow from '../chat/ReadGroupRow.svelte';
  import Icon from '../primitives/Icon.svelte';
  import AgentPaneStatusLine from './AgentPaneStatusLine.svelte';
  import AgentPaneComposerShell from './AgentPaneComposerShell.svelte';
  import {
    groupItemsBySubagent,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import { nodeKey } from '../chat/SubagentGroup.svelte';
  import { filterRedundantNotifications } from '../../utils/notificationFilter';

  interface Props {
    ctx: PanelContext;
  }

  let { ctx }: Props = $props();

  // Captured at init, NOT $derived: ctx.threadId is fixed for this instance
  // (CompanionPane keys the body on `${thread.id}:${kind}`), and
  // agentStateForPane writes the registry — illegal inside a derived. Same
  // constraint, same reason, as ReviewPane's review-state capture.
  // svelte-ignore state_referenced_locally
  const agent = ctx.threadId ? agentStateForPane(ctx.paneId, ctx.threadId) : null;

  let sourcePane = $derived(getPane(ctx.paneId));
  let rowPane = $derived(
    sourcePane
      ? (new Proxy(sourcePane, {
          get(target, prop) {
            if (prop === 'paneId') return `${target.paneId}~agent`;
            return Reflect.get(target, prop, target);
          },
        }) as ThreadPane)
      : undefined,
  );

  let scopeItemId = $derived(agent?.scopeItemId ?? '');
  let launch = $derived.by(() => {
    void ctx.timelineRevision;
    return scopeItemId ? ctx.getItemById(scopeItemId) : undefined;
  });

  // ---- Self-close (spec Q5) ------------------------------------------
  // Two exits: the breadcrumb popped to the root (empty scope — the pane
  // has nothing to show), or the scoped row VANISHED from a loaded
  // timeline (an edit-and-resend revert cut it). "Vanished" requires
  // having seen it: a launch merely outside the loaded window renders the
  // not-loaded state below instead of killing the pane.
  let seenScopeId = $state('');
  $effect(() => {
    if (!agent) return;
    if (scopeItemId === '') {
      ctx.closeAgentPane();
      return;
    }
    if (launch) {
      seenScopeId = scopeItemId;
      return;
    }
    if (untrack(() => seenScopeId) === scopeItemId && ctx.items.length > 0) {
      ctx.closeAgentPane();
    }
  });

  // ---- Scoped subtree -------------------------------------------------
  // Every loaded row whose parent chain reaches the scope. The completion
  // sibling of a NESTED launch has the scope (or a descendant) as its
  // parent, so it rides along and the grouping folds it onto its card; the
  // scope's OWN completion sibling lives outside the subtree and feeds the
  // status line instead.
  let scopedItems = $derived.by<Item[]>(() => {
    void ctx.timelineRevision;
    if (!scopeItemId) return [];
    const byParent = new Map<string, Item[]>();
    for (const item of ctx.items) {
      const pid = item.parentId;
      if (!pid) continue;
      let bucket = byParent.get(pid);
      if (!bucket) byParent.set(pid, (bucket = []));
      bucket.push(item);
    }
    const out: Item[] = [];
    const stack = [scopeItemId];
    while (stack.length > 0) {
      const kids = byParent.get(stack.pop()!);
      if (!kids) continue;
      for (const kid of kids) {
        out.push(kid);
        stack.push(kid.id);
      }
    }
    return out;
  });
  let completionItem = $derived.by(() => {
    void ctx.timelineRevision;
    if (!scopeItemId) return undefined;
    return ctx.items.find((item) => item.completionOf === scopeItemId);
  });
  let nodes = $derived(
    groupItemsBySubagent(
      filterRedundantNotifications(scopedItems),
      (anchorId) => sourcePane?.subagentLiveAggregate(anchorId),
    ),
  );

  // Scoping to a node whose settled children were evicted from pane memory
  // is exactly what hydrateChildren exists for. Re-armed whenever rows are
  // present, so a later eviction while the pane sits open re-hydrates once
  // instead of leaving an empty body.
  let hydratedScope = $state('');
  $effect(() => {
    if (!scopeItemId || !launch) return;
    if (scopedItems.length > 0) {
      hydratedScope = '';
      return;
    }
    if (untrack(() => hydratedScope) === scopeItemId) return;
    hydratedScope = scopeItemId;
    void ctx.ensureSubagentChildren(scopeItemId);
  });

  // ---- Follow the tail -------------------------------------------------
  // Plain stick-to-bottom: while the reader sits at the bottom, growth
  // keeps them there; scrolling up escapes. Deliberately not the chat
  // scroll controller — this is a companion scroller like the review
  // pane's, not a second timeline physics owner.
  let scrollEl: HTMLDivElement | undefined = $state();
  let pinned = $state(true);
  function onScroll(): void {
    if (!scrollEl) return;
    pinned = scrollEl.scrollHeight - scrollEl.scrollTop - scrollEl.clientHeight < 48;
  }
  $effect(() => {
    if (!scrollEl) return;
    const observer = new ResizeObserver(() => {
      if (pinned && scrollEl) scrollEl.scrollTop = scrollEl.scrollHeight;
    });
    observer.observe(scrollEl);
    for (const child of scrollEl.children) observer.observe(child);
    return () => observer.disconnect();
  });
  $effect(() => {
    void nodes;
    if (pinned && scrollEl) scrollEl.scrollTop = scrollEl.scrollHeight;
  });

  function openChild(itemId: string, label: string): void {
    ctx.openAgentScope(itemId, label);
    pinned = true;
  }
</script>

{#snippet renderNode(node: TimelineNode, depth: number)}
  {#if rowPane}
    {#if node.kind === 'leaf'}
      <TimelineLeaf pane={rowPane} item={node.item} orphan={node.orphan === true} />
    {:else if node.kind === 'group'}
      <SubagentGroup pane={rowPane} group={node} {depth} {renderNode} onOpenNode={openChild} />
    {:else if node.kind === 'wait_group'}
      <WaitGroup pane={rowPane} group={node} />
    {:else if node.kind === 'read_group'}
      <ReadGroupRow pane={rowPane} group={node} />
    {/if}
  {/if}
{/snippet}

<section
  class="flex h-full min-h-0 flex-col"
  data-testid="companion-pane-agent-body"
  aria-label="Agent Transcript"
>
  {#if agent && scopeItemId}
    <header class="flex items-center gap-2 border-b border-border px-3 py-2">
      <nav
        class="flex min-w-0 flex-1 items-center gap-1 text-sm"
        aria-label="Agent Scope"
        data-testid="agent-pane-breadcrumb"
      >
        {#each agent.breadcrumb as entry, index (entry.itemId)}
          {#if index > 0}
            <span class="shrink-0 text-fg-hint" aria-hidden="true">›</span>
          {/if}
          {#if index === agent.breadcrumb.length - 1}
            <span
              class="truncate font-medium text-text-primary"
              data-testid="agent-pane-breadcrumb-current"
            >
              {entry.label}
            </span>
          {:else}
            <button
              type="button"
              class="shrink-0 truncate rounded-[var(--radius-field)] px-1 text-fg-muted hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
              onclick={() => agent.popTo(index)}
              data-testid="agent-pane-breadcrumb-entry"
            >
              {entry.label}
            </button>
          {/if}
        {/each}
      </nav>
      <button
        type="button"
        class="shrink-0 rounded-[var(--radius-control)] p-1 text-fg-muted hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
        onclick={() => ctx.close()}
        aria-label="Close Agent Pane"
        data-testid="agent-pane-close"
      >
        <Icon icon={X} size={16} />
      </button>
    </header>

    <AgentPaneStatusLine {launch} completion={completionItem} hasChildren={scopedItems.length > 0} />

    <div
      bind:this={scrollEl}
      onscroll={onScroll}
      class="min-h-0 flex-1 overflow-y-auto overscroll-contain px-3 py-2"
      data-testid="agent-pane-timeline"
    >
      {#if !launch}
        <div class="flex h-full items-center justify-center px-4 text-center text-sm text-fg-subtle" data-testid="agent-pane-not-loaded">
          This agent's launch row isn't in the loaded timeline window.
        </div>
      {:else if nodes.length === 0}
        <div class="flex h-full items-center justify-center px-4 text-center text-sm text-fg-subtle" data-testid="agent-pane-empty">
          No output yet.
        </div>
      {:else}
        {#each nodes as node (nodeKey(node))}
          {@render renderNode(node, 0)}
        {/each}
      {/if}
    </div>

    {#if ctx.threadId}
      <AgentPaneComposerShell threadId={ctx.threadId} {launch} completion={completionItem} />
    {/if}
  {:else}
    <div class="flex h-full min-h-0 items-center justify-center px-4 text-sm text-fg-subtle">
      No agent scoped.
    </div>
  {/if}
</section>
