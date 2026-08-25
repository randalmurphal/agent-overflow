<script lang="ts">
  // Renderer for Claude's server-side `advisor` tool call. Runs
  // inline (not backgrounded) and behaves the same as a normal
  // tool_call/tool_completion pair in terms of lifecycle — the
  // distinguishing affordance is the brain icon, the model affix in
  // the header (e.g. "Advisor (Opus 4.7)"), and a prose-formatted
  // expanded body. The advisor runs in its OWN context window: tokens
  // are not surfaced on the parent's context meter (see
  // docs/references/claude-wire.md §server_tool_use). The model id is
  // stamped on `item.meta.advisor_model` at parse time and normalised
  // here via `displayModelLabel` — same pattern subagent rows use for
  // `subagent_model`.

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
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { formatDurationMs, formatTimeOfDay } from '../../utils/format';
  import { displayModelLabel } from '../../utils/modelLabels';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { importUnavailableLabel } from '../../utils/importUnavailable';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorWithFallback } from './rowState';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { createRunningElapsed } from './useRunningElapsed.svelte';
  import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';
  import { EMPTY_PATH_REFS, getPathRefsFromMeta } from '../../utils/pathLinkify';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';

  // Server-side preview is capped at 240 chars in
  // internal/triage/tool_lifecycle.go#completionPayload; the
  // collapsed-row affix caps further so the preview line fits
  // alongside the model affix and gutter chip without wrapping.
  const PREVIEW_MAX_CHARS = 80;

  let {
    pane,
    item,
  }: {
    pane?: PaneSession & RowUiRegistry & ScrollHost;
    item: Item;
  } = $props();

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

  let itemMeta = $derived(parseJsonObject(item.meta));
  let summaryMeta = $derived(parseJsonObject(item.payloadMeta));

  let advisorModel = $derived.by<string>(() => {
    const raw = itemMeta?.advisor_model;
    if (typeof raw !== 'string') return '';
    return raw.trim();
  });
  let modelLabel = $derived(advisorModel ? displayModelLabel('claude', advisorModel) : '');

  // Preview source order: full expansion data once loaded > the 240-char
  // `preview` field triage writes onto the tool_call_result payload meta
  // > nothing. `item.summary` is intentionally NOT a fallback — for
  // advisor calls triage sets it to the literal "advisor", which would
  // just duplicate the gutter chip if rendered as preview text.
  let preview = $derived.by<string>(() => {
    const stored = typeof summaryMeta?.preview === 'string' ? summaryMeta.preview : '';
    const source = expansion.displayData ?? stored;
    const trimmed = source.trim();
    if (trimmed.length <= PREVIEW_MAX_CHARS) return trimmed;
    return `${trimmed.slice(0, PREVIEW_MAX_CHARS)}…`;
  });

  let time = $derived(formatTimeOfDay(item.createdAt));

  const ticker = createRunningElapsed(
    () => item.status === 'running' || item.status === 'streaming',
    () => item.createdAt,
  );

  let durationMs = $derived.by<number | null>(() => {
    const d = summaryMeta?.durationMs;
    return typeof d === 'number' && d >= 0 ? d : null;
  });

    // One derived id for both halves of the disclosure: the header's
  // `controls` and the body's `id` must be the same string, and pane-scoped
  // (utils/chatDomIds.ts).
  let bodyDomId = $derived(chatRowDomId(pane, 'advisor-row-body', item.id));
let hasExpandableBody = $derived(Boolean(item.payloadId));

  let indicatorState = $derived(indicatorStateForItem(item, { meta: summaryMeta }));
  let rowError = $derived(
    rowErrorWithFallback(item, { meta: summaryMeta, fallback: 'Advisor call failed' }),
  );

  keepExpandedPayloadFresh(() => expansion, () => Boolean(item.payloadId));

  // Advisor body text is validated at persistToolCallCompletion time
  // (see internal/triage/tool_lifecycle.go). The validated allowlist
  // lives on item.meta.pathRefs alongside item.meta.advisor_model.
  const pathRefs = $derived(getPathRefsFromMeta(item.meta) ?? EMPTY_PATH_REFS);
</script>

<div class="group/tool overflow-hidden" data-testid="advisor-row" data-tool-kind="brain">
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    expandable={hasExpandableBody}
    controls={hasExpandableBody ? bodyDomId : undefined}
    testId="advisor-row-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 {hasExpandableBody ? 'hover:bg-surface-2/20' : ''}"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, () => expansion.toggle())}
  >
    {#snippet icon()}<ToolKindIcon kind="brain" ariaLabel="advisor" />{/snippet}
    {#snippet label()}<span data-testid="advisor-row-label">advisor</span>{/snippet}
    {#snippet body()}
      <span class="min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted/75" data-testid="advisor-row-preview">
        <span class="text-fg-muted">Advisor</span>{#if modelLabel}<span class="ml-1 text-fg-hint">({modelLabel})</span>{/if}{#if preview}<span class="ml-2">{preview}</span>{/if}
      </span>
    {/snippet}
    {#snippet actions()}
      <ToolDecisionChip decision={item.decision} />
      <ToolHeaderMeta
        statusSlotTestId="advisor-row-status-slot"
        duration={{
          testId: 'advisor-row-duration',
          label: durationMs !== null ? formatDurationMs(durationMs) : ticker.label,
        }}
        timestamp={{ testId: 'advisor-row-time', value: item.createdAt, label: time }}
      >
        {#snippet status()}
          <ToolRowStatusIndicator {item} state={indicatorState} testId="advisor-row-status" />
        {/snippet}
      </ToolHeaderMeta>
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
      testPrefix="advisor-row"
      emptyMessage={importUnavailableLabel(item) ?? 'No stored advisor response.'}
      copyLabel="Copy response"
      renderContent={advisorBodyContent}
    />
  {/if}
</div>

{#snippet advisorBodyContent({ data, testId }: { data: string; testId: string })}
  <div class="px-3 py-2 text-[0.75rem] leading-relaxed text-fg-muted" data-testid={testId}>
    <ChatMarkdown source={data} workspacePath={paneWorkspacePath(pane)} {pathRefs} />
  </div>
{/snippet}
