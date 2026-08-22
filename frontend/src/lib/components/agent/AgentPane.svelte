<script lang="ts">
  // The `agent` companion pane (docs/specs/agent-visibility.md Q4/Q5): a
  // READ-ONLY view of the source thread's transcript scoped to one
  // subagent launch. Unlike the inline card's digest body, this surface
  // shows everything the node produced — tool calls, thinking,
  // intermediate text, nested child cards.
  //
  // The body is the REAL MessageTimeline over a scoped ThreadPane facade
  // (stores/agentScopeView.svelte.ts): same virtualizer, same scroll
  // physics, same activity runs as the chat surface, with the facade's
  // override table naming every divergence (scoped items, own scroll
  // identity, no reveal gate, no edge paging). This pane owns only what
  // is NOT timeline: the breadcrumb, the composer shell (which carries
  // the run status and counters), scope lifecycle, and hydration of
  // evicted children.
  //
  // Scope changes swap in place (one pane per source pane, no stacking):
  // descending into a child card grows the breadcrumb (the facade routes
  // `openAgentPane` to pushScope), a breadcrumb click pops back, and
  // popping to the root leaves the empty scope, which this body answers
  // by closing the pane. The timeline is keyed on the scope id, so a
  // swap remounts it exactly like a thread switch.
  import { untrack } from 'svelte';
  import X from '@lucide/svelte/icons/x';
  import type { PanelContext } from '../../stores/panelContext.svelte';
  import { agentStateForPane } from '../../stores/agentPane.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import { createAgentScopeView } from '../../stores/agentScopeView.svelte';
  import MessageTimeline from '../chat/MessageTimeline.svelte';
  import Icon from '../primitives/Icon.svelte';
  import AgentPaneComposerShell from './AgentPaneComposerShell.svelte';
  import { decoratedSubagentAggregates } from '../../utils/subagentGrouping';
  import {
    codexCompletionAnswer,
    codexSubagentLaunchInfo,
    isCodexSubagentLaunchItem,
  } from '../../utils/subagentLaunch';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import {
    deriveClaudeSubagentDescription,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';

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
  let scopeItemId = $derived(agent?.scopeItemId ?? '');
  let launch = $derived.by(() => {
    void ctx.timelineRevision;
    return scopeItemId ? ctx.getItemById(scopeItemId) : undefined;
  });

  // One view per scope: created fresh when the scope (or source pane)
  // changes, disposed with it. The facade reads the scope id it was
  // built for, and the template keys the timeline on the same id.
  let view = $derived.by(() =>
    sourcePane && agent && scopeItemId
      ? createAgentScopeView(sourcePane, agent, scopeItemId)
      : null,
  );
  $effect(() => {
    const current = view;
    return () => current?.dispose();
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

  let scopedItems = $derived(view?.items ?? []);

  // At depth one the trail is "main › X" and the root entry is noise —
  // closing the pane IS "go back to main" (user ruling 2026-08-22).
  // Nested trails keep the full ancestry, root included, because there
  // the hops are real navigation. `crumbOffset` maps a rendered index
  // back to the trail index `popTo` expects.
  let visibleBreadcrumb = $derived.by(() => {
    const trail = agent?.breadcrumb ?? [];
    return trail.length === 2 ? trail.slice(1) : trail;
  });
  let crumbOffset = $derived((agent?.breadcrumb.length ?? 0) === 2 ? 1 : 0);

  // The launch's own one-line task next to the crumb ("Review frontend
  // agent-visibility…"), matching the card header. Claude: the input
  // description (prompt-truncation fallback included); Codex: the spawn
  // prompt, same 80-char clamp.
  let scopeDescription = $derived.by(() => {
    if (!launch) return '';
    if (isCodexSubagentLaunchItem(launch)) {
      const prompt = codexSubagentLaunchInfo(launch).prompt;
      return prompt.length > 80 ? `${prompt.slice(0, 80)}…` : prompt;
    }
    return deriveClaudeSubagentDescription(
      readClaudeSubagentInput(parseJsonObject(launch.payloadMeta), parseJsonObject(launch.meta)),
    );
  });
  let completionItem = $derived.by(() => {
    void ctx.timelineRevision;
    if (!scopeItemId) return undefined;
    return ctx.items.find((item) => item.completionOf === scopeItemId);
  });

  // A Codex child's final answer arrives on the spawn's completion
  // sibling (payloadMeta.preview) — none of the child's transcript
  // streams to the parent thread, so without this the pane for a
  // finished Codex agent says "No output yet." over a delivered verdict.
  let completionAnswer = $derived(codexCompletionAnswer(launch, completionItem));

  // Scoping to a node whose settled children were evicted from pane memory
  // is exactly what hydrateChildren exists for. Gate on the COUNT, the
  // same rule SubagentGroup uses on expand: eviction is partial — a
  // nested launch anchor survives (anchors are fold keys) while its
  // sibling rows page out, so "some rows loaded" proves nothing (e2e
  // regression: the pane opened from a collapsed, settled card rendered
  // only the nested card). The expected count is the larger of the
  // backend decoration and what the live-eviction fold says was paged
  // out. hydrateChildren dedupes in-flight and exhausted anchors itself,
  // so re-running is a no-op.
  $effect(() => {
    if (!scopeItemId || !launch) return;
    const evicted = sourcePane?.subagentLiveAggregate(scopeItemId)?.evictedCount ?? 0;
    const expected = Math.max(
      decoratedSubagentAggregates(launch).count,
      scopedItems.length + evicted,
    );
    if (scopedItems.length > 0 && scopedItems.length >= expected) return;
    void ctx.ensureSubagentChildren(scopeItemId);
  });
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
    <header class="flex items-center gap-2 border-b border-border px-3 py-2">
      <nav
        class="flex min-w-0 flex-1 items-center gap-1 text-sm"
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
          <span
            class="min-w-0 truncate text-xs text-fg-muted"
            data-testid="agent-pane-description"
          >
            {scopeDescription}
          </span>
        {/if}
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

    <div class="flex min-h-0 flex-1 flex-col" data-testid="agent-pane-timeline">
      {#if !launch}
        <div class="flex flex-1 items-center justify-center px-4 text-center text-sm text-fg-subtle" data-testid="agent-pane-not-loaded">
          This agent's launch row isn't in the loaded timeline window.
        </div>
      {:else if view && scopedItems.length > 0}
        <div class="min-h-0 flex-1">
          {#key scopeItemId}
            <MessageTimeline pane={view.pane} />
          {/key}
        </div>
      {:else if !completionAnswer}
        <div class="flex flex-1 items-center justify-center px-4 text-center text-sm text-fg-subtle" data-testid="agent-pane-empty">
          No output yet.
        </div>
      {/if}
      {#if launch && completionAnswer}
        <div class="min-h-0 shrink-0 overflow-y-auto px-3 py-2 text-sm" data-testid="agent-pane-final-answer">
          <ChatMarkdown source={completionAnswer} workspacePath={ctx.workspacePath ?? ''} />
        </div>
      {/if}
    </div>

    {#if ctx.threadId}
      <AgentPaneComposerShell
        threadId={ctx.threadId}
        {launch}
        completion={completionItem}
        hasChildren={scopedItems.length > 0}
      />
    {/if}
  {:else}
    <div class="flex h-full min-h-0 items-center justify-center px-4 text-sm text-fg-subtle">
      No agent scoped.
    </div>
  {/if}
</section>
