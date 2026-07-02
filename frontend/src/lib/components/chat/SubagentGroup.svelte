<script lang="ts" module>
  import type { TimelineNode as _TNode } from '../../utils/subagentGrouping';
  import { timelineNodeKey } from '../../utils/subagentGrouping';

  /**
   * Deterministic key for the `{#each}` binding. Item ids are only unique
   * within a thread, so the thread id is part of the key to prevent DOM
   * reuse between two different threads that both have text:1:0-style ids.
   */
  export function nodeKey(node: _TNode): string {
    return timelineNodeKey(node);
  }
</script>

<script lang="ts">
  // Visual structure mirrors `GenericToolCallRow.svelte` so a subagent
  // card reads as part of the timeline rather than a separate floating
  // callout. The only meaningful differences are:
  //   - expanded body is a scrollable list of children rendered via the
  //     parent-supplied `renderNode` snippet (no payload-fetch path);
  //   - title resolves from the parent tool_use input
  //     (`subagent_type` / `description` for Claude `Agent`);
  //   - per-subagent model is read from `parent.meta.subagent_model`,
  //     stamped by the Claude parser on the first subagent assistant
  //     envelope.
  // Status visualization generally mirrors a regular tool call.

  import type { Snippet } from 'svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { formatElapsedSeconds } from '../../utils/format';
  import {
    deriveClaudeSubagentDescription,
    deriveClaudeSubagentLabel,
    deriveClaudeSubagentModelLabel,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { SubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorForStatus } from './rowState';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';

  let {
    pane,
    group,
    depth = 0,
    renderNode,
  }: {
    /** Pane for the per-groupKey subagent expansion registry. When omitted,
     * falls back to local state — expand state then resets on windowing remount.
     * Real chat surfaces always pass `pane`. */
    pane?: ThreadPane;
    group: SubagentGroupNode;
    /**
     * Nesting depth of THIS group in the timeline tree:
     *   depth=1  first subagent card under a root item
     *   depth=2  a child subagent nested inside the first card
     *   depth=3  a grandchild — rendered as a marker only (spec cap)
     */
    depth?: number;
    /**
     * Snippet that knows how to render any TimelineNode. Provided by
     * the MessageTimeline so SubagentGroup does not take a hard
     * dependency on every leaf-rendering component. Also used
     * recursively for nested subagent groups.
     */
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  // Spec: render only a "Spawned subagent…" marker at depth >= 3
  // instead of another nested card. Stops the UI from displaying
  // three levels of nested collapsible boxes even when the underlying
  // data tree goes deeper.
  const GRANDCHILD_DEPTH_CAP = 3;
  const showMarkerOnly = $derived(depth >= GRANDCHILD_DEPTH_CAP);

  // Visual depth cap so wildly nested trees don't run off the right
  // edge. Grouping already limits structural depth; this just keeps
  // the indent budget sane. The outer rail wrapper in MessageTimeline
  // supplies the "top-level card" inset at depth=1, so adding our own
  // margin there would shift the chevron off-grid from adjacent tool
  // rows. Nested cards (depth ≥ 2) still indent so the parent/child
  // relationship reads visually inside the parent's expanded body.
  const indentRem = $derived(depth <= 1 ? 0 : Math.min(depth, 3) * 0.75);

  // Collapsed by default so large subagents don't dominate the
  // initial view. Persisted on the pane (keyed by group.groupKey) so the
  // user's expand state survives the window's overscan eviction. Local
  // fallback used only when `pane` is omitted (unit tests).
  let localExpanded = $state(false);
  const expanded = $derived(
    pane ? pane.isSubagentGroupExpanded(group.groupKey) : localExpanded,
  );

  function toggle(): void {
    if (pane) {
      pane.toggleSubagentGroupExpanded(group.groupKey);
    } else {
      localExpanded = !localExpanded;
    }
  }

  // History windows deliver launch anchors without their child rows —
  // the collapsed card renders from backend-decorated aggregates, and
  // the transcript hydrates on demand when the card expands. The pane
  // dedupes in-flight and completed loads per anchor id, so this effect
  // re-running on unrelated state is harmless.
  $effect(() => {
    if (!expanded || !pane) return;
    if (group.loadedDescendantCount >= group.descendantCount) return;
    void pane.ensureSubagentChildren(group.parent.id);
  });

  // ---- Header content derivations ---------------------------------

  let parent = $derived(group.parent);
  let parentMeta = $derived(parseJsonObject(parent.meta));
  let payloadMeta = $derived(parseJsonObject(parent.payloadMeta));
  let inputObject = $derived(readClaudeSubagentInput(payloadMeta, parentMeta));
  let parentToolName = $derived((parent.toolName ?? '').trim());

  // Title-cased label, model affix, and description — see
  // utils/claudeSubagentLabel.ts for the resolution rules. The
  // latest-action row below is always present and swaps from
  // "Initializing..." once child work lands.
  let agentTitle = $derived(deriveClaudeSubagentLabel(inputObject, parentToolName));
  let modelLabel = $derived(
    deriveClaudeSubagentModelLabel(inputObject, parentMeta, parentToolName),
  );
  let inputDescription = $derived(deriveClaudeSubagentDescription(inputObject));

  let previewText = $derived.by<string>(() => {
    return group.latestChildSummary || 'Initializing...';
  });

  // ---- Status visualization (matches GenericToolCallRow) -----------

  // The redesign drops the row-level "running" / "…" text label —
  // `Indicator` carries that state visually now. Keep a boolean
  // around for the per-second elapsed ticker and the "still running"
  // branches of the duration / status slots. The previous
  // `isBackgroundedLaunch` derivation existed only to pick between
  // the two unused strings, so it goes away with them.
  let isRunning = $derived(parent.status === 'running' || parent.status === 'streaming');

  let now = $state(Date.now());
  $effect(() => {
    if (!isRunning) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  let elapsedLabel = $derived.by<string>(() => {
    const start = parent.createdAt;
    if (!Number.isFinite(start) || start <= 0) return '';
    const end = isRunning ? now : parent.updatedAt;
    if (!Number.isFinite(end) || end <= start) return '';
    return formatElapsedSeconds(Math.floor((end - start) / 1_000));
  });

  let completionStatus = $derived(
    deriveCompletionStatus(parent, { meta: payloadMeta }),
  );
  let indicatorState = $derived(indicatorStateForItem(parent, { meta: payloadMeta }));
  let rowError = $derived.by(() => {
    if (completionStatus !== 'failure') return null;
    return rowErrorForStatus(parent.status, 'Agent failed') ?? {
      tone: 'error' as const,
      msg: 'Agent failed',
    };
  });

  let entryCountLabel = $derived.by(() => {
    if (group.descendantCount === 0) return '';
    return `${group.descendantCount} ${group.descendantCount === 1 ? 'entry' : 'entries'}`;
  });
  let entryCountAriaLabel = $derived.by(() => {
    if (group.descendantCount === 0) return '';
    return `${group.descendantCount} ${group.descendantCount === 1 ? 'timeline entry' : 'timeline entries'} inside this subagent group`;
  });
</script>

{#if showMarkerOnly}
  <div
    class="mb-2 flex items-center gap-2 text-xs italic text-text-secondary"
    style="margin-left: {indentRem}rem"
    data-testid="subagent-group-marker"
  >
    <span aria-hidden="true">↳</span>
    <span>Spawned subagent…{entryCountLabel ? ` (${entryCountLabel})` : ''}</span>
  </div>
{:else}
  <div
    class="group/tool overflow-hidden"
    style="margin-left: {indentRem}rem"
    data-testid="subagent-group"
    data-tool-kind="robot"
  >
    <TranscriptDisclosureHeader
      expanded={expanded}
      controls={`subagent-group-${parent.id}`}
      testId="subagent-group-toggle"
      class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
      onToggle={(event) => preservePaneScrollAnchor(pane, event, toggle)}
    >
      {#snippet icon()}<ToolKindIcon kind="robot" ariaLabel="agent" />{/snippet}
      {#snippet label()}<span data-testid="subagent-group-gutter-label">agent</span>{/snippet}
      {#snippet body()}
      <span class="min-w-0 flex-1">
        <span class="flex min-w-0 items-center gap-2">
          <span
            class="text-[0.75rem] text-fg-muted shrink-0"
            data-testid="subagent-group-label"
          >
            {agentTitle}{#if modelLabel}<span class="ml-1 text-fg-hint normal-case tracking-normal">({modelLabel})</span>{/if}
          </span>
          {#if inputDescription}
            <span class="min-w-0 truncate text-[0.75rem] text-fg-muted/75" data-testid="subagent-group-description">
              {inputDescription}
            </span>
          {/if}
        </span>
        <span class="mt-0.5 block min-w-0 truncate text-[0.6875rem] text-fg-hint/85" data-testid="subagent-group-preview">
          <span aria-hidden="true">└</span>
          {previewText}
        </span>
      </span>
      {/snippet}
      {#snippet actions()}
        {#if entryCountLabel}
          <span
            class="shrink-0 text-[0.625rem] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100"
            data-testid="subagent-group-count"
            aria-label={entryCountAriaLabel}
          >
            {entryCountLabel}
          </span>
        {/if}
        <ToolHeaderMeta
          statusSlotTestId="subagent-group-status-slot"
          duration={{ testId: 'subagent-group-duration', label: elapsedLabel }}
        >
          {#snippet status()}
            <ToolRowStatusIndicator
              item={parent}
              state={isRunning || completionStatus === 'failure' ? indicatorState : null}
              testId="subagent-group-status"
            />
          {/snippet}
        </ToolHeaderMeta>
      {/snippet}
    </TranscriptDisclosureHeader>

    {#if rowError}
      <div class="ml-[5.25rem] px-3 pb-1" data-testid="subagent-group-error">
        <RowError tone={rowError.tone} msg={rowError.msg} />
      </div>
    {/if}

    {#if expanded}
      <div
        id="subagent-group-{parent.id}"
        class="ml-5 max-h-[20rem] overflow-y-auto border-l border-border-subtle bg-surface-0/35 px-3 py-2"
        role="region"
        aria-label="Subagent Timeline"
        data-testid="subagent-group-body"
      >
        {#if group.children.length === 0}
          {#if group.descendantCount > 0}
            <p class="text-xs text-text-secondary italic" data-testid="subagent-group-loading">
              Loading {entryCountLabel}…
            </p>
          {:else}
            <p class="text-xs text-text-secondary italic">No child entries captured.</p>
          {/if}
        {:else}
          {#each group.children as child (nodeKey(child))}
            {@render renderNode(child, depth + 1)}
          {/each}
        {/if}
      </div>
    {/if}
  </div>
{/if}
