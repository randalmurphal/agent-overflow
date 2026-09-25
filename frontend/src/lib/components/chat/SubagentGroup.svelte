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
  //   - no status pills: `data-background` marks a detached node for
  //     tests, and a pending approval shows ONLY in the composer's
  //     approval UI (user ruling 2026-08-23), never on the card;
  //   - live progress (tool count, activity line, tokens) from
  //     `provider:subagent_progress`, falling back to the final numbers
  //     triage persisted on the launch row at terminal;
  //   - expanded body is a capped, virtualized DIGEST of the node's tool
  //     calls and final text. Thinking, intermediate text, and child-agent
  //     navigation live in the agent pane.
  //   - a PARKED stop's card (utils/parkedStop.ts) says the agent reported
  //     and what it waits on, shows its run's duration and report head,
  //     and expands to the full report above the run's digest.
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
    type SubagentGroupNode,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import {
    claudeResumeCarrierIdentity,
    completionAnswerPreview,
    codexSubagentLaunchInfo,
    codexSubagentTaskDescription,
    isClaudeResumeCarrierItem,
    isCodexAgentLaunchItem,
    launchRunsDetached,
    subagentLaunchInfo,
    type SubagentLaunchContext,
  } from '../../utils/subagentLaunch';
  import { parkedStopFromItem, parkedStopSentence } from '../../utils/parkedStop';
  import { liveSubagentProgress } from '../../stores/subagentProgress.svelte';
  import {
    formatToolUses,
    resolveSubagentProgress,
  } from '../../utils/subagentProgress';
  import { threadHasScope } from '../../transport/entityScopes';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem } from './rowState';
  import { subagentCardOutputError, subagentCardRowError, subagentCardSpan } from './subagentCardStatus';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import SubagentCardActions from './SubagentCardActions.svelte';
  import SubagentGroupBody from './SubagentGroupBody.svelte';
  import { displayModelLabel } from '../../utils/modelLabels';

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
  let navigationOnly = $derived(Boolean(pane?.agentScopeRootId));
  const expanded = $derived(
    navigationOnly ? false : pane ? pane.isSubagentGroupExpanded(group.groupKey) : localExpanded,
  );

  function toggle(): void {
    if (navigationOnly) return;
    if (pane) {
      pane.toggleSubagentGroupExpanded(group.groupKey);
    } else {
      localExpanded = !localExpanded;
    }
  }

  // ---- Header content derivations ---------------------------------

  // Resolve at the row boundary, exactly like `TimelineLeaf` — the node
  // tree is a STRUCTURAL snapshot rebuilt per `timelineRevision`, so
  // everything on this card that moves inside a turn (parent status,
  // entry count, the latest-action preview) is read from the store here
  // rather than patched into the node upstream. Doing it upstream made
  // every streaming tick of every group descendant rebuild the whole
  // projection — grouping, run wrapping and the virtualizer's data array
  // for ~800 rows, at up to 48Hz.
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
  let parent = $derived(pane?.getItemById(group.parent.id)
    ?? completionItem?.completionLaunch
    ?? group.parent);
  let statusItem = $derived(completionItem ?? parent);
  // The report line, read off the stop this card sits at (Codex
  // FINAL_ANSWER, Claude output-file report, a parked stop's report head).
  let completionAnswer = $derived(completionAnswerPreview(parent, completionItem));
  let parkedStop = $derived(parkedStopFromItem(completionItem));
  // The main timeline holds no child rows, so a collapsed card's count and
  // preview come from the backend decoration on the live anchor, which
  // triage re-pushes as the children are written.
  let decorated = $derived(decoratedSubagentAggregates(parent, completionItem));
  // Max, not replace: the same reconciliation `subagentGroupNode` does,
  // re-run against the live anchor. The node's count covers the children a
  // scoped window loads and whatever decoration existed when it was built;
  // only the decoration can move without a structural bump. Taking the max
  // picks up a decoration that lands mid-turn and falls back to the
  // structural count (never to zero) if a later upsert arrives without one.
  let descendantCount = $derived(Math.max(group.descendantCount, decorated.count));
  // The backend ranks a card's children; the node's build-time preview
  // stands in only for a write that carries no decoration at all.
  let latestChildSummary = $derived(decorated.present ? decorated.summary : group.latestChildSummary);
  // One derived id for both halves of the disclosure (utils/chatDomIds.ts):
  // the header's `controls` and the body's `id` must be one string.
  let groupDomId = $derived(chatRowDomId(pane, 'subagent-group', group.anchor.id));
  let parentMeta = $derived(parseJsonObject(parent.meta));
  let payloadMeta = $derived(parseJsonObject(parent.payloadMeta));
  let statusPayloadMeta = $derived(
    completionItem ? parseJsonObject(completionItem.payloadMeta) : payloadMeta,
  );
  let inputObject = $derived(readClaudeSubagentInput(payloadMeta, parentMeta));
  let parentToolName = $derived((parent.toolName ?? '').trim());

  // The provider-neutral launch identity: kind chip, display name,
  // async-ness. The context answers "does this launch have children?"
  // from the node (built from the rows the window holds) and the live
  // anchor's decoration. A group node whose row somehow stops answering
  // the predicate (cannot happen for the kinds the grouping mints, but the
  // type allows it) falls back to a plain foreground agent presentation
  // rather than a blank header.
  const launchCtx: SubagentLaunchContext = {
    hasChildren: () => group.children.length > 0 || descendantCount > 0,
  };
  let identityItem = $derived(parent.toolName === 'collab_agent' && completionItem
    ? { ...parent, meta: completionItem.meta } : parent);
  let launchInfo = $derived(subagentLaunchInfo(identityItem, launchCtx));
  let kindLabel = $derived(launchInfo?.kind ?? 'agent');
  let agentTitle = $derived(launchInfo?.name ?? (parentToolName || 'Agent'));
  let modelLabel = $derived.by(() => {
    if (launchInfo?.model) {
      const model = displayModelLabel(launchInfo.provider, launchInfo.model);
      return launchInfo.reasoningEffort ? `${model} ${launchInfo.reasoningEffort}` : model;
    }
    const named = deriveClaudeSubagentModelLabel(inputObject, parentMeta, parentToolName);
    if (named) return named;
    if (launchInfo?.provider !== 'claude') return '';
    const inherited = pane?.effectiveModel || pane?.thread?.model || '';
    return inherited ? displayModelLabel('claude', inherited) : '';
  });
  // The one-line task beside the title. Codex spawns read their OWN
  // shape (V1's plaintext prompt; V2 adds nothing, because its prompt is
  // encrypted and the label already brackets the task name);
  // a resume carrier reads the ORIGINAL agent's description off its
  // stamped meta (its SendMessage input only names the recipient id);
  // everything else reads the Claude input block.
  let inputDescription = $derived(
    isCodexAgentLaunchItem(parent)
      ? codexSubagentTaskDescription(codexSubagentLaunchInfo(parent))
      : isClaudeResumeCarrierItem(parent)
        ? claudeResumeCarrierIdentity(parent).description
        : deriveClaudeSubagentDescription(inputObject),
  );

  // ---- Status visualization (matches GenericToolCallRow) -----------

  let isRunning = $derived(
    statusItem.status === 'running' || statusItem.status === 'streaming',
  );

  // ---- Live progress (spec Q1) --------------------------------------
  // The live tick while the agent runs; the persisted final numbers once
  // it settled. `isRunning` (completion-aware) is passed as the liveness
  // override because the launch row of a background agent never leaves
  // `running` — see resolveSubagentProgress.
  let liveTick = $derived(isRunning ? liveSubagentProgress(parent.threadId, parent.id) : undefined);
  let progress = $derived(resolveSubagentProgress(parent, completionItem, liveTick, isRunning));
  let toolCountLabel = $derived(formatToolUses(progress.toolUses));
  let tokensLabel = $derived(
    progress.totalTokens !== null ? `${formatTokens(progress.totalTokens)} tokens` : '',
  );

  // "background" pill: the node runs detached from the main turn — stamped
  // at launch (§E5 async, run_in_background, Codex spawn) or mid-flight
  // (the background button / Ctrl+B, which lands as
  // `meta.subagentBackgroundedAt`).
  let isBackgroundNode = $derived(launchRunsDetached(launchInfo, parentMeta));

  // Background button (spec Q9): Claude foreground Agent/Task only, while
  // it runs. Forks have no task to detach, a resume carrier is already
  // background, and Codex children are always async. BackgroundClaudeTask
  // rides `threads:operate`; a session without it gets no button.
  let canBackground = $derived(
    pane !== undefined
      && isRunning
      && !isBackgroundNode
      && threadHasScope('threads:operate', parent.threadId)
      && (parentToolName === 'Agent' || parentToolName === 'Task'),
  );
  let backgroundError = $state('');

  let previewText = $derived.by<string>(() => {
    // The live activity line is the freshest statement of what the agent
    // is doing right now (`task_progress.description`); the decorated
    // child summary and the Initializing placeholder are the fallbacks.
    if (isRunning && progress.activity) return progress.activity;
    // A finished agent's answer is its collapsed line (Codex FINAL_ANSWER,
    // Claude output-file report); the last progress message is not what a
    // reader wants from a finished agent.
    if (completionAnswer) return completionAnswer;
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

  let completionStatus = $derived(
    deriveCompletionStatus(statusItem, { meta: statusPayloadMeta }),
  );
  let indicatorState = $derived(
    indicatorStateForItem(statusItem, { meta: statusPayloadMeta }),
  );
  let statusMeta = $derived(
    completionItem ? parseJsonObject(completionItem.meta) : parentMeta,
  );

  let elapsedLabel = $derived.by<string>(() => {
    const { start, end } = subagentCardSpan({
      parent, statusItem, completionMeta: completionItem ? statusMeta : null,
      parkedStop, running: isRunning, now: isRunning ? clock.now : 0,
    });
    if (Number.isFinite(start) && start > 0 && Number.isFinite(end) && end > start) {
      return formatElapsedSeconds(Math.floor((end - start) / 1_000));
    }
    // A settled row with unusable timestamps (an imported session) still
    // has the provider's own wall-clock report in the persisted progress.
    return progress.durationMs !== null ? formatDurationMs(progress.durationMs) : '';
  });

  let rowError = $derived(subagentCardRowError(statusItem, completionStatus, statusMeta));
  let outputBackfillError = $derived(subagentCardOutputError(statusMeta));

  let entryCountLabel = $derived.by(() => {
    if (descendantCount === 0) return '';
    return `${descendantCount} ${descendantCount === 1 ? 'entry' : 'entries'}`;
  });
  let entryCountAriaLabel = $derived.by(() => {
    if (descendantCount === 0) return '';
    return `${descendantCount} ${descendantCount === 1 ? 'timeline entry' : 'timeline entries'} inside this subagent group`;
  });

  // ---- Expanded-body digest (spec Q2; user ruling 2026-08-23) --------
  // The allowlist lives in utils/subagentDigest.ts and the body in
  // SubagentDigestBody, shared with the background tray row. What this
  // card decides is whether the agent's latest text is an answer: while
  // it runs (the latest text is its live report) or after a clean
  // completion. A killed/errored agent's last text is mid-flight prose
  // (user report 2026-08-22: a stopped agent's prose rendered in the main
  // chat history), and a forked Skill publishes its synthetic answer as a
  // top-level sourced result. The mirrored assistant row stays in the
  // agent pane so the main timeline never duplicates the answer above and
  // below the activity boundary. A parked card shows its run's report above
  // the digest, so the digest drops it.
  let parkedReportId = $derived(parkedStop?.reportItemId ?? '');
  let keepFinalText = $derived(
    parentMeta?.directCommandResult !== true && !parkedReportId && (isRunning || completionStatus !== 'failure'),
  );
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
    data-anchor-id={group.anchor.id}
    data-status={statusItem.status}
    data-background={isBackgroundNode ? 'true' : undefined}
  >
    {#snippet cardMetrics()}
      {#if toolCountLabel}
        <span
          class="shrink-0 text-[0.625rem] text-fg-hint tabular-nums"
          data-testid="subagent-group-tools"
        >
          {toolCountLabel}
        </span>
      {/if}
      {#if toolCountLabel && tokensLabel}<span aria-hidden="true">·</span>{/if}
      {#if tokensLabel}
        <span
          class="shrink-0"
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
    {/snippet}
    {#snippet cardDetails()}
      {#if parkedStop}
        <span class="block truncate text-[0.6875rem] italic text-fg-subtle" data-testid="subagent-group-parked-status">
          {parkedStopSentence(parkedStop)}
        </span>
      {/if}
      {#if previewText}
        <button
          type="button"
          tabindex="-1"
          disabled={navigationOnly}
          onclick={(event) => preservePaneScrollAnchor(pane, event, toggle)}
          class="block w-full truncate bg-transparent p-0 text-left text-[0.6875rem] text-fg-hint/85 disabled:cursor-default"
          data-testid="subagent-group-preview"
          title={previewText}
        >
          {previewText}
        </button>
      {/if}
    {/snippet}
    <TranscriptDisclosureHeader
      agentLayout
      metrics={toolCountLabel || tokensLabel || entryCountLabel ? cardMetrics : undefined}
      details={previewText || parkedStop ? cardDetails : undefined}
      expanded={expanded}
      expandable={!navigationOnly}
      controls={groupDomId}
      ariaLabel={`Toggle ${kindLabel} ${agentTitle}${inputDescription ? `: ${inputDescription}` : ''}`}
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
            class="min-w-0 truncate text-[0.75rem] text-fg-muted"
            title={agentTitle + (modelLabel ? ` (${modelLabel})` : '')}
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
      </span>
      {/snippet}
      {#snippet actions()}
        <SubagentCardActions {pane} {parent} {agentTitle} {canBackground} {navigationOnly} bind:backgroundError />
        <ToolHeaderMeta
          statusSlotTestId="subagent-group-status-slot"
          duration={{ testId: 'subagent-group-duration', label: elapsedLabel }}
        >
          {#snippet status()}
            <ToolRowStatusIndicator
              item={statusItem}
              state={isRunning || parkedStop || completionStatus === 'failure' ? indicatorState : null}
              testId="subagent-group-status"
            />
          {/snippet}
        </ToolHeaderMeta>
      {/snippet}
    </TranscriptDisclosureHeader>

    {#if rowError}
      <div class="ml-[5.25rem] compact:ml-5 px-3 pb-1" data-testid="subagent-group-error">
        <RowError tone={rowError.tone} msg={rowError.msg} />
      </div>
    {/if}
    {#if backgroundError}
      <div class="ml-[5.25rem] compact:ml-5 px-3 pb-1" data-testid="subagent-group-background-error">
        <RowError tone="error" msg={backgroundError} />
      </div>
    {/if}
    {#if outputBackfillError}
      <div class="ml-[5.25rem] compact:ml-5 px-3 pb-1" data-testid="subagent-group-output-error">
        <RowError tone="error" msg={outputBackfillError} />
      </div>
    {/if}

    {#if expanded}
      <SubagentGroupBody {pane} {group} {parent} {completionItem} id={groupDomId}
        reportItemId={parkedReportId} {descendantCount} {entryCountLabel} {keepFinalText}
        live={isRunning} {depth} {renderNode} />
    {/if}
  </div>
{/if}
