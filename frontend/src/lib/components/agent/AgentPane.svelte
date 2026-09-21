<script lang="ts">
  import { untrack } from 'svelte';
  import X from '@lucide/svelte/icons/x';
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import { agentStateForPane } from '../../stores/agentPane.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import { createAgentScopeView, type AgentScopeView } from '../../stores/agentScopeView.svelte';
  import MessageTimeline from '../chat/MessageTimeline.svelte';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import PaneHeaderLine from '../panes/PaneHeaderLine.svelte';
  import { companionForSource } from '../../stores/companionPanes.svelte';
  import AgentPaneComposerShell from './AgentPaneComposerShell.svelte';
  import {
    codexSubagentLaunchInfo,
    codexSubagentTaskDescription,
    isCodexSubagentLaunchItem,
  } from '../../utils/subagentLaunch';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import {
    deriveClaudeSubagentDescription,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';

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
  let sourceThreadId = $derived(sourcePane?.threadId);
  // ctx.paneId is the SOURCE pane. Focus is held by this companion's own
  // layout pane, so the header's focus mark keys on that id.
  let companionPaneId = $derived(companionForSource(ctx.paneId, 'agent')?.paneId ?? null);
  let scopeItemId = $derived(agent?.scopeItemId ?? '');
  let view = $state.raw<AgentScopeView | null>(null);
  $effect(() => {
    const source = sourcePane;
    const scope = scopeItemId;
    if (!source || !sourceThreadId || !agent || !scope) return;
    const current = untrack(() => createAgentScopeView(source, scope, {
      viewKey: 'agent',
      openAgentPane: (id, label) => agent.pushScope(id, label),
    }));
    view = current;
    untrack(() => current.start());
    return () => { current.dispose(); if (view === current) view = null; };
  });
  let lastItemRequest = 0;
  $effect(() => {
    const current = view;
    const request = agent?.itemRequest;
    if (!current || current.pane.loading || !request?.itemId || request.nonce === lastItemRequest) return;
    lastItemRequest = request.nonce;
    untrack(() => current.pane.requestScrollToItem(request.itemId));
  });
  let launch = $derived(view?.root);
  let scopedItems = $derived(view?.items ?? []);
  $effect(() => {
    if (agent && (!scopeItemId || view?.gone)) ctx.closeAgentPane();
  });

  // At depth one the trail is "main › X" and the root entry is noise —
  // closing the pane IS "go back to main" (user ruling 2026-08-22).
  // Nested trails keep the full ancestry, root included, because there
  // the hops are real navigation. `crumbOffset` maps a rendered index
  // back to the trail index `popTo` expects.
  // The stored label is what the row said when the pane opened. A Codex
  // child's nickname and profile land on its spawn row after the spawn,
  // so a loaded Codex launch supplies its live label.
  let visibleBreadcrumb = $derived.by(() => {
    void ctx.timelineRevision;
    const trail = (agent?.breadcrumb ?? []).map((entry) => {
      const row = entry.itemId === scopeItemId ? launch : entry.itemId ? ctx.getItemById(entry.itemId) : undefined;
      if (!row || !isCodexSubagentLaunchItem(row)) return entry;
      return { ...entry, label: codexSubagentLaunchInfo(row).agentLabel };
    });
    return trail.length === 2 ? trail.slice(1) : trail;
  });
  let crumbOffset = $derived((agent?.breadcrumb.length ?? 0) === 2 ? 1 : 0);

  // The launch's own one-line task next to the crumb ("Review frontend
  // agent-visibility…"), matching the card header. Claude: the input
  // description (prompt-truncation fallback included); Codex: the spawn
  // prompt on V1, and nothing on V2 — its prompt is encrypted and the
  // crumb already carries the model-chosen task name it falls back to.
  let scopeDescription = $derived.by(() => {
    if (!launch) return '';
    if (isCodexSubagentLaunchItem(launch)) {
      return codexSubagentTaskDescription(codexSubagentLaunchInfo(launch));
    }
    return deriveClaudeSubagentDescription(
      readClaudeSubagentInput(parseJsonObject(launch.payloadMeta), parseJsonObject(launch.meta)),
    );
  });
  let completionItem = $derived.by(() => {
    void ctx.timelineRevision;
    if (!scopeItemId) return undefined;
    // Membership from the array (structure); the row's live fields from
    // its box — an in-place patch to the row never fires the array signal.
    const completion = ctx.items.find((item) => item.completionOf === scopeItemId);
    return completion ? (ctx.getItemById(completion.id) ?? completion) : undefined;
  });

  // Identity and LIFECYCLE are two rows for a resumed agent (§E6): the
  // scope root is what the agent IS (name, description, model, the
  // hydration gate), the view's lifecycle row is what it is DOING right
  // now — the latest resume carrier while a later round runs. The view
  // owns that resolution so the timeline's turn and the shell's run
  // state cannot disagree; the fallbacks here cover only the degenerate
  // case of a context with no registered source pane.
  let lifecycleItem = $derived(view ? view.lifecycle : launch);
  let lifecycleCompletionItem = $derived(view ? view.lifecycleCompletion : completionItem);

</script>

<!-- bg-surface-0: CompanionPane paints its bodies bg-surface-1 (elevated
     chrome), but this body IS a thread transcript — it must sit on the
     same ground as the chat timeline or the pane reads as a different
     surface entirely. -->
<section
  class="flex h-full min-h-0 flex-col bg-surface-0"
  data-testid="companion-pane-agent-body"
  aria-label="Agent Transcript"
>
  {#if agent && scopeItemId}
    <!-- Same chrome as ChatHeader: padding, separator, h-5 icon button, and
         the relative z-10 stacking context that keeps the separator above the
         timeline's top fade (which overdraws its clip by one pixel). The nav's
         py-0.5 matches the chat title button's box so both headers measure
         the same height. -->
    <header
      class="relative z-10 flex shrink-0 items-center gap-2 border-b border-border-subtle px-5 py-2 min-w-0"
      data-testid="agent-pane-header"
    >
      {#if companionPaneId}
        <PaneHeaderLine paneId={companionPaneId} />
      {/if}
      <nav
        class="flex min-w-0 flex-1 items-center gap-1 py-0.5 text-sm"
        aria-label="Agent Scope"
        data-testid="agent-pane-breadcrumb"
      >
        {#each visibleBreadcrumb as entry, index (entry.itemId)}
          {#if index > 0}
            <span class="shrink-0 text-fg-hint" aria-hidden="true">›</span>
          {/if}
          {#if index === visibleBreadcrumb.length - 1}
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
              onclick={() => agent.popTo(index + crumbOffset)}
              data-testid="agent-pane-breadcrumb-entry"
            >
              {entry.label}
            </button>
          {/if}
        {/each}
        {#if scopeDescription}
          <span class="shrink-0 text-fg-hint" aria-hidden="true">-</span>
          <span
            class="min-w-0 truncate text-xs text-fg-muted"
            data-testid="agent-pane-description"
          >
            {scopeDescription}
          </span>
        {/if}
      </nav>
      <PaneHeaderIconButton label="Close Agent Pane" testId="agent-pane-close" onclick={() => ctx.close()}>
        <Icon icon={X} size={12} strokeWidth={2} />
      </PaneHeaderIconButton>
    </header>

    <div class="flex min-h-0 flex-1 flex-col" data-testid="agent-pane-timeline" aria-busy={!view || view.pane.loading}>
      {#if view?.error}
        <div class="px-4 py-3 text-sm text-text-secondary" role="alert">
          <p>{view.error}</p>
          <button class="mt-2 text-accent" onclick={() => view?.pane.retryHistoryLoad()}>Retry</button>
        </div>
      {/if}
      {#if view && (view.pane.loading || scopedItems.length > 0)}
        <div class="min-h-0 flex-1">
          {#key scopeItemId}
            <MessageTimeline pane={view.pane} />
          {/key}
        </div>
      {:else if view && !view.error}
        <div class="flex flex-1 items-center justify-center px-4 text-center text-sm text-fg-subtle" data-testid="agent-pane-empty">
          No output yet.
        </div>
      {/if}
    </div>

    {#if ctx.threadId}
      <AgentPaneComposerShell
        threadId={ctx.threadId}
        pane={sourcePane}
        {launch}
        lifecycle={lifecycleItem}
        lifecycleCompletion={lifecycleCompletionItem}
        hasChildren={scopedItems.length > 0}
      />
    {/if}
  {:else}
    <div class="flex h-full min-h-0 items-center justify-center px-4 text-sm text-fg-subtle">
      No agent scoped.
    </div>
  {/if}
</section>
