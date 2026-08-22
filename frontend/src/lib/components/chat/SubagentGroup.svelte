<script lang="ts" module>
  import type { TimelineNode as _TNode } from '../../utils/subagentGrouping';
  import { timelineNodeKey } from '../../utils/subagentGrouping';
  import { chatRowDomId } from '../../utils/chatDomIds';

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
  // THE shared agent card (docs/specs/agent-visibility.md Q1/Q2/Q6): one
  // component renders every launch kind — Claude Agent/Task (awaited or
  // background), a forked Skill, a SendMessage resume carrier, a Codex
  // spawn — in the timeline and in the agent pane. Visual structure
  // mirrors `GenericToolCallRow.svelte` so it reads as part of the
  // timeline rather than a separate floating callout. What is specific
  // to this card:
  //   - kind chip (`agent` / `skill`) + name from the provider-neutral
  //     launch predicate (`utils/subagentLaunch.ts`);
  //   - status pills: `background` for async nodes, `needs approval`
  //     while an interactive request scoped to this launch is pending;
  //   - live progress (tool count, activity line, tokens-when-room) from
  //     `provider:subagent_progress`, falling back to the final numbers
  //     triage persisted on the launch row at terminal;
  //   - expanded body is a DIGEST: the node's tool calls, its final
  //     text, and collapsed child cards. Thinking and intermediate text
  //     live in the agent pane (Q2) — which is also why the body has no
  //     height cap or fade: what remains is short by construction.
  //   - open-in-pane button; a background button while a foreground
  //     Claude agent runs (Q9).

  import type { Snippet } from 'svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { formatDurationMs, formatElapsedSeconds, formatTokens } from '../../utils/format';
  import { createSharedNowClock } from './useRunningElapsed.svelte';
  import {
    deriveClaudeSubagentDescription,
    deriveClaudeSubagentModelLabel,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    decoratedSubagentAggregates,
    pickLatestChildSummary,
    type SubagentGroupNode,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import {
    codexCompletionAnswer,
    subagentLaunchInfo,
    type SubagentLaunchContext,
  } from '../../utils/subagentLaunch';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import { paneWorkspacePath } from '../../stores/thread.svelte';
  import { liveSubagentProgress } from '../../stores/subagentProgress.svelte';
  import {
    formatToolUses,
    resolveSubagentProgress,
  } from '../../utils/subagentProgress';
  import { BackgroundClaudeTask } from '../../stores/bindings';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorForStatus } from './rowState';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import Icon from '../primitives/Icon.svelte';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import SendToBack from '@lucide/svelte/icons/send-to-back';

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
    if (group.loadedDescendantCount >= descendantCount) return;
    void pane.ensureSubagentChildren(group.parent.id);
  });

  // ---- Header content derivations ---------------------------------

  // Resolve at the row boundary, exactly like `TimelineLeaf` — the node
  // tree is a STRUCTURAL snapshot rebuilt per `timelineRevision`, so
  // everything on this card that moves inside a turn (parent status,
  // entry count, the latest-action preview) is read from the store here
  // rather than patched into the node upstream. Doing it upstream made
  // every streaming tick of every group descendant rebuild the whole
  // projection — grouping, run wrapping and the virtualizer's data array
  // for ~800 rows, at up to 48Hz.
  let parent = $derived(pane?.getItemById(group.parent.id) ?? group.parent);
  // A backgrounded launch row stays `running` forever by design (the tray
  // invariant: the launch is immutable and the outcome arrives on a separate
  // `complete:<id>` sibling). The grouping folds that sibling onto the node,
  // and it — not the launch — is what says whether this agent finished, how
  // it finished, and when. An awaited launch has no sibling and completes in
  // place, so the launch row remains the source there.
  let completionItem = $derived(
    group.completion
      ? (pane?.getItemById(group.completion.id) ?? group.completion)
      : null,
  );
  let statusItem = $derived(completionItem ?? parent);
  // The Codex child's delivered verdict (empty for Claude launches).
  let completionAnswer = $derived(codexCompletionAnswer(parent, completionItem));
  let decorated = $derived(decoratedSubagentAggregates(parent));
  // Max, not replace — the same reconciliation `subagentGroupNode` does,
  // re-run against the live anchor. The node's count already folds in
  // loaded children, the eviction fold, and whatever decoration existed
  // when it was built; only the decoration can move without a structural
  // bump. Taking the max picks up a decoration that lands mid-turn and
  // falls back to the structural count (never to zero) if a later upsert
  // arrives without one.
  let descendantCount = $derived(Math.max(group.descendantCount, decorated.count));
  let latestChildSummary = $derived(
    pickLatestChildSummary(
      group.children,
      pane?.subagentLiveAggregate(parent.id),
      (id) => pane?.getItemById(id),
    )
      || decorated.summary
      || group.latestChildSummary,
  );
  // One derived id for both halves of the disclosure (utils/chatDomIds.ts):
  // the header's `controls` and the body's `id` must be one string.
  let groupDomId = $derived(chatRowDomId(pane, 'subagent-group', parent.id));
  let parentMeta = $derived(parseJsonObject(parent.meta));
  let payloadMeta = $derived(parseJsonObject(parent.payloadMeta));
  let statusPayloadMeta = $derived(
    completionItem ? parseJsonObject(completionItem.payloadMeta) : payloadMeta,
  );
  let inputObject = $derived(readClaudeSubagentInput(payloadMeta, parentMeta));
  let parentToolName = $derived((parent.toolName ?? '').trim());

  // The provider-neutral launch identity: kind chip, display name,
  // async-ness. The context answers "does this launch have loaded
  // children?" from the node itself — the group was BUILT from the rows
  // the window holds, so no second index is needed. A group node whose
  // row somehow stops answering the predicate (cannot happen for the
  // kinds the grouping mints, but the type allows it) falls back to a
  // plain foreground agent presentation rather than a blank header.
  const launchCtx: SubagentLaunchContext = {
    hasChildren: () => group.children.length > 0 || group.descendantCount > 0,
  };
  let launchInfo = $derived(subagentLaunchInfo(parent, launchCtx));
  let kindLabel = $derived(launchInfo?.kind ?? 'agent');
  let agentTitle = $derived(launchInfo?.name ?? (parentToolName || 'Agent'));
  let modelLabel = $derived(
    deriveClaudeSubagentModelLabel(inputObject, parentMeta, parentToolName),
  );
  let inputDescription = $derived(deriveClaudeSubagentDescription(inputObject));

  // ---- Status visualization (matches GenericToolCallRow) -----------

  let isRunning = $derived(
    statusItem.status === 'running' || statusItem.status === 'streaming',
  );

  // ---- Live progress (spec Q1) --------------------------------------
  // The live tick while the agent runs; the persisted final numbers once
  // it settled. `isRunning` (completion-aware) is passed as the liveness
  // override because the launch row of a background agent never leaves
  // `running` — see resolveSubagentProgress.
  let liveTick = $derived(liveSubagentProgress(parent.threadId, parent.id));
  let progress = $derived(resolveSubagentProgress(parent, liveTick, isRunning));
  let toolCountLabel = $derived(formatToolUses(progress.toolUses));
  let tokensLabel = $derived(
    progress.totalTokens !== null ? `${formatTokens(progress.totalTokens)} tokens` : '',
  );

  // "needs approval" pill (spec Q10b): an interactive request whose scope
  // is THIS launch. Direct scope only — a nested agent's ask lights the
  // nested card, which is visible inside this card's body when expanded.
  let needsApproval = $derived.by(() => {
    if (!pane) return false;
    const scoped = (requests: readonly { parentToolUseId?: string }[] | undefined) =>
      (requests ?? []).some((request) => request.parentToolUseId === parent.id);
    return scoped(pane.pendingApprovals) || scoped(pane.pendingUserInputs);
  });

  // "background" pill: the node runs detached from the main turn — stamped
  // at launch (§E5 async, run_in_background, Codex spawn) or mid-flight
  // (the background button / Ctrl+B, which lands as
  // `meta.subagentBackgroundedAt`).
  let isBackgroundNode = $derived(
    launchInfo?.background === true || parentMeta?.subagentBackgroundedAt !== undefined,
  );

  // Background button (spec Q9): Claude foreground Agent/Task only, while
  // it runs. Forks have no task to detach, a resume carrier is already
  // background, and Codex children are always async.
  let canBackground = $derived(
    pane !== undefined
      && isRunning
      && !isBackgroundNode
      && (parentToolName === 'Agent' || parentToolName === 'Task'),
  );
  let backgrounding = $state(false);
  let backgroundError = $state('');

  async function moveToBackground(event: MouseEvent): Promise<void> {
    event.stopPropagation();
    if (backgrounding) return;
    backgrounding = true;
    backgroundError = '';
    try {
      await BackgroundClaudeTask(parent.threadId, parent.id);
    } catch (err) {
      // The CLI's refusal ("no matching foreground task") is a real
      // answer the user needs to see — the row keeps streaming.
      backgroundError = err instanceof Error ? err.message : String(err);
    } finally {
      backgrounding = false;
    }
  }

  // One door: the PANE decides where opening routes. The base ThreadPane
  // opens/rescopes its agent companion (the trail restarts — the
  // from-outside rule); the agent pane's scoped facade overrides
  // `openAgentPane` to pushScope, so descending INSIDE the pane grows the
  // breadcrumb (spec Q4b). Rows never talk to the companion store.
  function openInPane(event: MouseEvent): void {
    event.stopPropagation();
    pane?.openAgentPane(parent.id, agentTitle);
  }

  let previewText = $derived.by<string>(() => {
    // The live activity line is the freshest statement of what the agent
    // is doing right now (`task_progress.description`); child summaries
    // and the Initializing placeholder are the fallbacks.
    if (isRunning && progress.activity) return progress.activity;
    if (latestChildSummary) return latestChildSummary;
    const activityConfirmed =
      descendantCount > 0 || inputObject?.run_in_background === false;
    return activityConfirmed && isRunning ? 'Initializing...' : '';
  });

  // Shared 1Hz clock (useRunningElapsed.svelte.ts) instead of a private
  // per-row interval: N running subagent cards tick one interval and one
  // state write per second, not N cascades. The completed branch keeps
  // its local parent.updatedAt math, which the shared label helper does
  // not model.
  const clock = createSharedNowClock(() => isRunning);

  let elapsedLabel = $derived.by<string>(() => {
    const start = parent.createdAt;
    if (Number.isFinite(start) && start > 0) {
      // Start at the launch, end at whatever carries the terminal — for a
      // background agent that is the completion sibling, whose updatedAt is
      // when the task actually reported back.
      const end = isRunning ? clock.now : statusItem.updatedAt;
      if (Number.isFinite(end) && end > start) {
        return formatElapsedSeconds(Math.floor((end - start) / 1_000));
      }
    }
    // A settled row with unusable timestamps (an imported session) still
    // has the provider's own wall-clock report in the persisted progress.
    return progress.durationMs !== null ? formatDurationMs(progress.durationMs) : '';
  });

  let completionStatus = $derived(
    deriveCompletionStatus(statusItem, { meta: statusPayloadMeta }),
  );
  let indicatorState = $derived(
    indicatorStateForItem(statusItem, { meta: statusPayloadMeta }),
  );
  let rowError = $derived.by(() => {
    if (completionStatus !== 'failure') return null;
    return rowErrorForStatus(statusItem.status, 'Agent failed') ?? {
      tone: 'error' as const,
      msg: 'Agent failed',
    };
  });

  // A failed transcript backfill (the task_notification's output_file
  // could not be read — triage stamps notification_output_state/error on
  // the completion sibling, output_file_state/error on older rows). A
  // silently incomplete card body reads exactly like a complete one, so
  // the failure renders inline.
  let statusMeta = $derived(
    completionItem ? parseJsonObject(completionItem.meta) : parentMeta,
  );
  let outputBackfillError = $derived.by(() => {
    const state = statusMeta?.notification_output_state ?? statusMeta?.output_file_state;
    if (state !== 'error') return '';
    const error = statusMeta?.notification_output_error ?? statusMeta?.output_file_error;
    return typeof error === 'string' && error ? error : 'Task output could not be read.';
  });

  let entryCountLabel = $derived.by(() => {
    if (descendantCount === 0) return '';
    return `${descendantCount} ${descendantCount === 1 ? 'entry' : 'entries'}`;
  });
  let entryCountAriaLabel = $derived.by(() => {
    if (descendantCount === 0) return '';
    return `${descendantCount} ${descendantCount === 1 ? 'timeline entry' : 'timeline entries'} inside this subagent group`;
  });

  // ---- Expanded-body digest (spec Q2) --------------------------------
  // The body shows the node's tool calls, its FINAL text, and collapsed
  // child cards. Thinking and intermediate text live in the agent pane —
  // that filter is also what keeps the body short enough to render
  // uncapped (the old max-height + fade scroller is deleted, Q6).
  let bodyNodes = $derived.by<TimelineNode[]>(() => {
    let lastTextId = '';
    for (const node of group.children) {
      if (node.kind === 'leaf' && node.item.kind === 'assistant_text') {
        lastTextId = node.item.id;
      }
    }
    return group.children.filter((node) => {
      if (node.kind !== 'leaf') return true;
      const kind = node.item.kind;
      if (kind === 'thinking') return false;
      if (kind === 'assistant_text') return node.item.id === lastTextId;
      return true;
    });
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
    class="group/tool @container overflow-hidden"
    style="margin-left: {indentRem}rem"
    data-testid="subagent-group"
    data-tool-kind="robot"
  >
    <TranscriptDisclosureHeader
      expanded={expanded}
      controls={groupDomId}
      testId="subagent-group-toggle"
      class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
      onToggle={(event) => preservePaneScrollAnchor(pane, event, toggle)}
    >
      {#snippet icon()}<ToolKindIcon kind="robot" ariaLabel={kindLabel} />{/snippet}
      {#snippet label()}<span data-testid="subagent-group-kind">{kindLabel}</span>{/snippet}
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
        {#if previewText}
          <span class="mt-0.5 block min-w-0 truncate text-[0.6875rem] text-fg-hint/85" data-testid="subagent-group-preview">
            <span aria-hidden="true">└</span>
            {previewText}
          </span>
        {/if}
      </span>
      {/snippet}
      {#snippet actions()}
        {#if needsApproval}
          <span
            class="shrink-0 rounded-full border border-warning/30 bg-warning/10 px-1.5 py-0.5 text-[0.625rem] font-medium text-warning"
            data-testid="subagent-group-approval-pill"
          >
            needs approval
          </span>
        {/if}
        {#if isBackgroundNode}
          <span
            class="shrink-0 rounded-full border border-border-subtle bg-surface-2/40 px-1.5 py-0.5 text-[0.625rem] font-medium text-text-secondary"
            data-testid="subagent-group-background-pill"
          >
            background
          </span>
        {/if}
        {#if toolCountLabel}
          <span
            class="shrink-0 text-[0.625rem] text-fg-hint tabular-nums"
            data-testid="subagent-group-tools"
          >
            {toolCountLabel}
          </span>
        {/if}
        {#if tokensLabel}
          <!-- Tokens only when the card has room (spec Q1): container
               width, not viewport — a narrow pane on a wide screen still
               hides them. -->
          <span
            class="hidden shrink-0 text-[0.625rem] text-fg-hint tabular-nums @[36rem]:inline"
            data-testid="subagent-group-tokens"
          >
            {tokensLabel}
          </span>
        {/if}
        {#if entryCountLabel}
          <span
            class="shrink-0 text-[0.625rem] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100"
            data-testid="subagent-group-count"
            aria-label={entryCountAriaLabel}
          >
            {entryCountLabel}
          </span>
        {/if}
        {#if canBackground}
          <button
            type="button"
            onclick={moveToBackground}
            disabled={backgrounding}
            title="Move to background"
            aria-label="Move agent to background"
            data-testid="subagent-group-background-button"
            class="opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100 rounded p-0.5 text-text-secondary hover:text-text-primary cursor-pointer disabled:cursor-default disabled:opacity-40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            <Icon icon={SendToBack} size={12} />
          </button>
        {/if}
        {#if pane}
          <button
            type="button"
            onclick={openInPane}
            title="Open in agent pane"
            aria-label="Open {agentTitle} in agent pane"
            data-testid="subagent-group-open-pane"
            class="opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100 rounded p-0.5 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          >
            <Icon icon={PanelRightOpen} size={12} />
          </button>
        {/if}
        <ToolHeaderMeta
          statusSlotTestId="subagent-group-status-slot"
          duration={{ testId: 'subagent-group-duration', label: elapsedLabel }}
        >
          {#snippet status()}
            <ToolRowStatusIndicator
              item={statusItem}
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
    {#if backgroundError}
      <div class="ml-[5.25rem] px-3 pb-1" data-testid="subagent-group-background-error">
        <RowError tone="error" msg={backgroundError} />
      </div>
    {/if}
    {#if outputBackfillError}
      <div class="ml-[5.25rem] px-3 pb-1" data-testid="subagent-group-output-error">
        <RowError tone="error" msg={outputBackfillError} />
      </div>
    {/if}

    {#if expanded}
      <div
        id={groupDomId}
        class="ml-5 border-l border-border-subtle bg-surface-0/35 px-3 py-2"
        role="region"
        aria-label="Subagent Timeline"
        data-testid="subagent-group-body"
      >
        {#if group.children.length === 0}
          {#if descendantCount > 0}
            <p class="text-xs text-text-secondary italic" data-testid="subagent-group-loading">
              Loading {entryCountLabel}…
            </p>
          {:else if !completionAnswer}
            <p class="text-xs text-text-secondary italic">No child entries captured.</p>
          {/if}
        {:else if bodyNodes.length === 0}
          {#if !completionAnswer}
            <p class="text-xs text-text-secondary italic" data-testid="subagent-group-digest-empty">
              Intermediate output only. Open the agent pane for the full transcript.
            </p>
          {/if}
        {:else}
          {#each bodyNodes as child (nodeKey(child))}
            {@render renderNode(child, depth + 1)}
          {/each}
        {/if}
        {#if completionAnswer}
          <!-- A Codex child streams nothing to the parent thread; the
               FINAL_ANSWER on the folded completion sibling is its whole
               product, so the card body is where it renders. -->
          <div class="text-sm" data-testid="subagent-group-final-answer">
            <ChatMarkdown source={completionAnswer} workspacePath={paneWorkspacePath(pane)} />
          </div>
        {/if}
      </div>
    {/if}
  </div>
{/if}
