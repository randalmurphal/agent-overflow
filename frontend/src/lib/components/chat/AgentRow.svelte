<script lang="ts">
  // Compact agent LEAF row. Three shapes share it:
  //
  //   - The launch row of a DETACHED Claude launch (`launchRunsDetached`:
  //     the §E5 async ack, run_in_background, a SendMessage resume
  //     carrier, or backgrounded mid-flight). This is the pre-card
  //     background launch row, unchanged by the agent-visibility work
  //     except for the open-in-pane door (user ruling 2026-08-23, the
  //     one approved change): robot icon, label, model, description, the
  //     `backgrounded` indicator the status rule gives a running
  //     background launch (rowState.ts), the launch time. No ticker (the
  //     launch stays `running` forever) and no text pill (c58f9b55).
  //     Everything the agent does shows on its card at the completion
  //     point (`SubagentGroupNode.anchor`); this row never reads the
  //     completion sibling.
  //   - The background tray's agent entry: the host supplies the
  //     completion as `statusItem`, its own duration and its actions.
  //   - The two page-boundary shapes: a depth-cap flattened launch and a
  //     completion sibling whose launch is outside the loaded window.
  //
  // The inline timeline renders awaited launches as the shared agent card
  // (SubagentGroup); this row is deliberately header-only. The old
  // expandable body that parsed the payload JSONL back into a
  // pseudo-transcript (ClaudeSubagentTranscript) is DELETED
  // (docs/specs/agent-visibility.md migration table, "ack-text
  // rendering"): the agent's transcript exists as real rows under the
  // launch — streamed live or backfilled from the task_notification's
  // output_file — and renders in the card body and the agent pane. A
  // failed output-file read/backfill still surfaces here as an inline
  // error line, because a silently incomplete transcript reads exactly
  // like a complete one.

  import type { Snippet } from 'svelte';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import type { Item } from '../../types/models';
  import { paneWorkspacePath } from '../../stores/thread.svelte';
  import type {
    PaneSession,
    RowUiRegistry,
    PaneDoors,
    TimelineSource,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import Icon from '../primitives/Icon.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { presentToolCardInputPreview } from './toolCardPreview';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { formatDurationMs, formatTimeOfDay } from '../../utils/format';
  import {
    deriveClaudeSubagentDescription,
    deriveClaudeSubagentLabel,
    deriveClaudeSubagentModelLabel,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import {
    agentScopeRootId,
    claudeResumeCarrierIdentity,
    isClaudeResumeCarrierItem,
  } from '../../utils/subagentLaunch';
  import { displayModelLabel } from '../../utils/modelLabels';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorWithFallback } from './rowState';
  import { createRunningElapsed } from './useRunningElapsed.svelte';

  let {
    pane,
    item,
    displayItem,
    statusItem,
    durationLabel = '',
    showTimestamp = true,
    hostActions,
  }: {
    pane?: PaneDoors & PaneSession & RowUiRegistry & ScrollHost & TimelineSource;
    item: Item;
    displayItem?: Item;
    statusItem?: Item;
    durationLabel?: string;
    showTimestamp?: boolean;
    /** A host's own row actions (the tray's stop/open). Replaces the open-pane door. */
    hostActions?: Snippet;
  } = $props();

  // The launch this row stands for. A completion sibling names it through
  // `completionOf`; every other shape IS the launch.
  let launchId = $derived(item.kind === 'tool_completion' ? (item.completionOf ?? '') : item.id);
  // A completion leaf (page boundary: its launch outside the loaded
  // window, or a depth-cap flatten) renders with its LAUNCH's identity
  // when the launch is loaded. The completion item stays the status source.
  let launchItem = $derived(
    item.kind === 'tool_completion' && launchId ? pane?.getItemById(launchId) : undefined,
  );
  let effectiveDisplayItem = $derived(displayItem ?? launchItem ?? item);
  let effectiveStatusItem = $derived(statusItem ?? item);
  let summaryMeta = $derived(parseJsonObject(effectiveDisplayItem.payloadMeta));
  let displayMeta = $derived(parseJsonObject(effectiveDisplayItem.meta));
  let itemMeta = $derived(parseJsonObject(item.meta));
  let statusMeta = $derived(parseJsonObject(effectiveStatusItem.payloadMeta));
  let agentToolName = $derived(effectiveDisplayItem.toolName === 'Task' ? 'Agent' : (effectiveDisplayItem.toolName ?? 'Agent'));
  let agentInputObject = $derived(readClaudeSubagentInput(summaryMeta, displayMeta));
  // A §E6 resume carrier's identity (type / model / description) lives in
  // its stamped meta, not its SendMessage input — the input only names
  // the recipient agent id, which is noise to a reader.
  let carrierIdentity = $derived(
    isClaudeResumeCarrierItem(effectiveDisplayItem)
      ? claudeResumeCarrierIdentity(effectiveDisplayItem)
      : null,
  );
  let agentLabel = $derived(
    carrierIdentity?.name ?? deriveClaudeSubagentLabel(agentInputObject, agentToolName),
  );
  let modelLabel = $derived(
    carrierIdentity
      ? (carrierIdentity.model ? displayModelLabel('claude', carrierIdentity.model) : '')
      : deriveClaudeSubagentModelLabel(agentInputObject, displayMeta, agentToolName),
  );
  let description = $derived(
    carrierIdentity ? carrierIdentity.description : deriveClaudeSubagentDescription(agentInputObject),
  );
  let inputPreview = $derived(
    description ||
      presentToolCardInputPreview(effectiveDisplayItem, summaryMeta, displayMeta, paneWorkspacePath(pane)).text,
  );
  let time = $derived(formatTimeOfDay(effectiveStatusItem.createdAt));

  // A backgrounded launch never leaves `running`, so it gets no elapsed
  // ticker; its indicator is the static `backgrounded` dot.
  let isBackgroundedLaunch = $derived(
    effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true,
  );
  let isRunning = $derived(effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming');
  const ticker = createRunningElapsed(
    () => isRunning && durationLabel === '' && !isBackgroundedLaunch,
    () => item.createdAt,
  );
  let durationMs = $derived.by<number | null>(() => {
    const d = summaryMeta?.durationMs;
    return typeof d === 'number' && d >= 0 ? d : null;
  });
  // The output-file / transcript-backfill state triage stamped on this row.
  // Only the ERROR is rendered — 'loading' and 'loaded' describe payload
  // availability the card and pane own now.
  let deferredOutputError = $derived.by(() => {
    const state = itemMeta?.notification_output_state ?? itemMeta?.output_file_state;
    if (state !== 'error') return '';
    const error = itemMeta?.notification_output_error ?? itemMeta?.output_file_error;
    return typeof error === 'string' && error ? error : 'Task output could not be read.';
  });
  let indicatorState = $derived(indicatorStateForItem(effectiveStatusItem, { meta: statusMeta }));
  let rowError = $derived(
    rowErrorWithFallback(effectiveStatusItem, { meta: statusMeta, fallback: 'Agent failed' }),
  );

  // Same door the card uses: the PANE decides where opening routes (the
  // base pane opens its agent companion; the scoped facade pushes a
  // breadcrumb hop). A host that supplies its own actions (the background
  // tray) keeps them; the default is the open affordance, so the launch
  // row is the way into a running background agent's transcript.
  let opensAgentPane = $derived(pane !== undefined && hostActions === undefined && launchId !== '');
  // The scope is the TRANSCRIPT ROOT, which is this launch except on a
  // §E6 resume carrier: Claude parents every resumed round's rows to the
  // ORIGINAL launch, so scoping to the carrier would open an empty pane.
  // The label stays this row's, which is the agent the reader clicked.
  let paneScopeId = $derived.by(() => {
    const launchRow = item.kind === 'tool_completion' ? launchItem : item;
    return launchRow ? agentScopeRootId(launchRow) : launchId;
  });
  function openInPane(event: MouseEvent): void {
    event.stopPropagation();
    pane?.openAgentPane(paneScopeId, agentLabel);
  }
</script>

{#snippet rowActions()}
  {#if hostActions}
    {@render hostActions()}
  {:else if opensAgentPane}
    <button
      type="button"
      onclick={openInPane}
      title="Open in agent pane"
      aria-label="Open {agentLabel} in agent pane"
      data-testid="agent-row-open-pane"
      class="inline-flex items-center justify-center opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100 rounded p-0.5 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      <Icon icon={PanelRightOpen} size={12} />
    </button>
  {/if}
{/snippet}

<div class="group/tool overflow-hidden" data-testid="agent-row" data-tool-kind="robot">
  <TranscriptDisclosureHeader
    expanded={false}
    expandable={false}
    testId="agent-row-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1"
  >
    {#snippet icon()}<ToolKindIcon kind="robot" ariaLabel="agent" />{/snippet}
    {#snippet label()}<span data-testid="agent-row-label">agent</span>{/snippet}
    {#snippet body()}
      <span class="min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted/75" data-testid="agent-row-preview">
        <span class="text-fg-muted">{agentLabel}</span>{#if modelLabel}<span class="ml-1 text-fg-hint">({modelLabel})</span>{/if}{#if inputPreview}<span class="ml-2">{inputPreview}</span>{/if}
      </span>
    {/snippet}
    {#snippet actions()}
      <ToolDecisionChip decision={item.decision} />
      <ToolHeaderMeta
        statusSlotTestId="agent-row-status-slot"
        duration={{
          testId: 'agent-row-duration',
          label: durationLabel || (durationMs !== null ? formatDurationMs(durationMs) : ticker.label),
        }}
        timestamp={showTimestamp ? { testId: 'agent-row-time', value: effectiveStatusItem.createdAt, label: time } : undefined}
        actions={hostActions || opensAgentPane ? rowActions : undefined}
      >
        {#snippet status()}
          <ToolRowStatusIndicator item={effectiveStatusItem} state={indicatorState} testId="agent-row-status" />
        {/snippet}
      </ToolHeaderMeta>
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if rowError}
    <div class="ml-[5.25rem] px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}
  {#if deferredOutputError}
    <div class="ml-[5.25rem] px-3 pb-1" data-testid="agent-row-output-error">
      <RowError tone="error" msg={deferredOutputError} />
    </div>
  {/if}
</div>
