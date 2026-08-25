<script lang="ts">
  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import { paneWorkspacePath } from '../../stores/thread.svelte';
  import type {
    PaneSession,
    RowUiRegistry,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { classifyToolName } from './toolCardHeader';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { importUnavailableLabel } from '../../utils/importUnavailable';
  import { presentToolCardInputPreview } from './toolCardPreview';
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';
  import EditorLink from '../common/EditorLink.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { formatDurationMs, formatTimeOfDay } from '../../utils/format';
  import { isCodexSubagentLaunchItem } from '../../utils/subagentLaunch';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorWithFallback } from './rowState';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { createRunningElapsed } from './useRunningElapsed.svelte';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';

  let {
    pane,
    item,
    displayItem,
    statusItem,
    durationLabel = '',
    showTimestamp = true,
    hostActions,
  }: {
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    item: Item;
    displayItem?: Item;
    statusItem?: Item;
    durationLabel?: string;
    showTimestamp?: boolean;
    hostActions?: Snippet;
  } = $props();
  let effectiveDisplayItem = $derived(displayItem ?? item);
  let effectiveStatusItem = $derived(statusItem ?? item);

  let classification = $derived(classifyToolName(effectiveDisplayItem.toolName ?? effectiveDisplayItem.summary));
  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => item.payloadId,
          () => item.threadId,
          { payloadVersion: () => item.updatedAt },
        ),
  );
  const expansionRef = useLeasedItemExpansion({
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => localFallback,
  });
  const expansion = $derived(expansionRef.current!);

  let summaryMeta = $derived(parseJsonObject(effectiveDisplayItem.payloadMeta));
  let displayMeta = $derived(parseJsonObject(effectiveDisplayItem.meta));
  let itemMeta = $derived(parseJsonObject(item.meta));
  // Auto-denied before the provider could ask (Claude
  // `system/permission_denied`). Triage stamps the reason onto the tool row
  // alongside decision='declined'; the full notice is its own timeline row.
  let permissionDenialReason = $derived.by(() => {
    const denied = itemMeta?.permissionDenied;
    if (!denied || typeof denied !== 'object' || Array.isArray(denied)) return '';
    const reason = (denied as Record<string, unknown>).reason;
    return typeof reason === 'string' ? reason.trim() : '';
  });
  let statusMeta = $derived(parseJsonObject(effectiveStatusItem.payloadMeta));

  let time = $derived(formatTimeOfDay(effectiveStatusItem.createdAt));

  let preview = $derived(
    presentToolCardInputPreview(effectiveDisplayItem, summaryMeta, displayMeta, paneWorkspacePath(pane)),
  );
  let previewClass = $derived(
    preview.path
      ? 'min-w-0 flex-1 whitespace-normal break-all text-[0.75rem] leading-4 text-fg-muted/75'
      : 'min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted/75',
  );

  let durationMs = $derived.by<number | null>(() => {
    if (!summaryMeta) return null;
    const d = summaryMeta.durationMs;
    if (typeof d === 'number' && d >= 0) return d;
    return null;
  });

  let isBackgroundedLaunch = $derived(
    effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true,
  );
  let isCodexSubagentLaunch = $derived(isCodexSubagentLaunchItem(effectiveStatusItem));

  // The redesign drops the row-level "running" / "…" text label —
  // `Indicator` carries that state visually now. The original derived
  // string survives as a boolean gate for the per-second elapsed-time
  // ticker: only running, non-backgrounded, non-Codex-subagent rows
  // need the interval.
  let isRunning = $derived(
    !isCodexSubagentLaunch
      && (effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming'),
  );

  let indicatorState = $derived(indicatorStateForItem(effectiveStatusItem, { meta: statusMeta }));
  let rowError = $derived(
    rowErrorWithFallback(effectiveStatusItem, { meta: statusMeta, fallback: 'Tool call failed' }),
  );
  const ticker = createRunningElapsed(
    () => isRunning && durationLabel === '' && !isBackgroundedLaunch,
    () => item.createdAt,
  );

  let deferredOutputState = $derived.by(() => {
    if (!itemMeta) return '';
    const state = itemMeta.notification_output_state ?? itemMeta.output_file_state;
    return typeof state === 'string' ? state : '';
  });

  let deferredOutputError = $derived.by(() => {
    if (!itemMeta) return '';
    const error = itemMeta.notification_output_error ?? itemMeta.output_file_error;
    return typeof error === 'string' ? error : '';
  });

  let suppressBodyExpansion = $derived(item.toolName === 'TaskOutput' || item.toolName === 'Read' || item.toolName === 'Skill');
    // One derived id for both halves of the disclosure: the header's
  // `controls` and the body's `id` must be the same string, and pane-scoped
  // (utils/chatDomIds.ts).
  let bodyDomId = $derived(chatRowDomId(pane, 'tool-call-card-body', item.id));
let hasExpandableBody = $derived(
    !suppressBodyExpansion &&
      (Boolean(item.payloadId) ||
        deferredOutputState === 'loading' ||
        deferredOutputState === 'error'),
  );

  keepExpandedPayloadFresh(
    () => expansion,
    () => Boolean(item.payloadId),
  );

  async function toggle() {
    await expansion.toggle();
  }

</script>

{#snippet headerActions()}
  <ToolDecisionChip decision={item.decision} reason={permissionDenialReason} />
  <ToolHeaderMeta
    statusSlotTestId="tool-call-card-status-slot"
    duration={{
      testId: 'tool-call-card-duration',
      label: durationLabel || (durationMs !== null ? formatDurationMs(durationMs) : ticker.label),
    }}
    timestamp={showTimestamp
      ? { testId: 'tool-call-card-time', value: effectiveStatusItem.createdAt, label: time }
      : undefined}
    actions={hostActions}
  >
    {#snippet status()}
      <ToolRowStatusIndicator item={effectiveStatusItem} state={indicatorState} testId="tool-call-card-status" />
    {/snippet}
  </ToolHeaderMeta>
{/snippet}

<div
  class="group/tool overflow-hidden"
  data-testid="tool-call-card"
  data-tool-kind={classification.icon}
>
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    expandable={hasExpandableBody}
    controls={hasExpandableBody ? bodyDomId : undefined}
    testId="tool-call-card-toggle"
    interactiveBody={preview.path !== undefined}
    class="rounded-[var(--radius-control)] px-1 py-1 {hasExpandableBody ? 'hover:bg-surface-2/20' : ''}"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, toggle)}
  >
    {#snippet icon()}<ToolKindIcon kind={classification.icon} ariaLabel={classification.label} />{/snippet}
    {#snippet label()}<span data-testid="tool-call-card-label">{classification.label}</span>{/snippet}
    {#snippet body()}
      <span class={previewClass} data-testid="tool-call-card-preview">
        {#if preview.path}
          <EditorLink
            path={preview.path.path}
            line={preview.path.line ?? 0}
            col={preview.path.col ?? 0}
            workspacePath={paneWorkspacePath(pane)}
            label={preview.text}
            openLabel={preview.text}
            stopPropagation
            tone="inherit"
            class="max-w-full break-all hover:text-accent focus-visible:text-accent"
          />
        {:else}
          {preview.text}
        {/if}
      </span>
    {/snippet}
    {#snippet actions()}
      {@render headerActions()}
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if rowError}
    <div class="ml-[5.25rem] px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}

  {#if hasExpandableBody && expansion.expanded}
    <ExpandablePayloadBody
      {pane}
      {expansion}
      id={bodyDomId}
      testPrefix="tool-call-card"
      emptyMessage={importUnavailableLabel(item) ?? 'No stored payload for this tool result.'}
      {deferredOutputState}
      {deferredOutputError}
    />
  {/if}
</div>
