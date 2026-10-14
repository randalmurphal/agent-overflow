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
    TimelineSource,
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
  import {
    aoToolFacts,
    aoToolPresentation,
    aoToolServer,
    computerName as aoComputerName,
    REMOTE_TOOLS_SERVER,
    THREAD_TOOLS_SERVER,
    type AoToolNames,
  } from './aoTools';
  import { aoToolError, isSettledRequestState } from './aoToolResults';
  import { answeredRequestTokens } from '../../utils/answeredRequestTokens';
  import {
    readItemProjectionSource,
    requestItemProjectionSource,
  } from '../../utils/itemProjectionSource.svelte';
  import { getThreadById, getThreads } from '../../stores/threads.svelte';
  import { getEffectiveThreadStatus } from '../../stores/threadStatuses.svelte';
  import { getWorkflowRun } from '../../stores/workflowRuns.svelte';
  import AoToolBody from './AoToolBody.svelte';
  import { BROWSER_TOOLS_SERVER } from '../../utils/browserTools';
  import {
    attachedBackendEntry,
    backendDisplayName,
    threadMachine,
  } from '../../stores/attachedBackends.svelte';
  import { previewRouted } from '../../stores/devServers.svelte';
  import { browserCompanionState } from '../../stores/browserCompanion.svelte';
  import {
    attachRemoteJobs,
    remoteJobComputerName,
    remoteJobRecord,
  } from '../../stores/remoteJobs.svelte';

  let {
    pane,
    item,
    displayItem,
    statusItem,
    durationLabel = '',
    showTimestamp = true,
    hostActions,
  }: {
    pane?: PaneSession & RowUiRegistry & ScrollHost & Partial<Pick<TimelineSource, 'items'>>;
    item: Item;
    displayItem?: Item;
    statusItem?: Item;
    durationLabel?: string;
    showTimestamp?: boolean;
    hostActions?: Snippet;
  } = $props();
  let effectiveDisplayItem = $derived(displayItem ?? item);
  let effectiveStatusItem = $derived(statusItem ?? item);

  let displayMeta = $derived(parseJsonObject(effectiveDisplayItem.meta));
  // A tool AO itself serves (remote, browser) presents by its own table:
  // family icon, verb, the argument that matters. Everything else goes by
  // tool name. The ids its input carries are shown by name: a computer this
  // client is attached to, or one the thread's job listing names; a job by
  // its label; a page by its label or title.
  let aoServer = $derived(aoToolServer(displayMeta));
  $effect(() => {
    if (aoServer !== REMOTE_TOOLS_SERVER || !item.threadId) return;
    const held = attachRemoteJobs(item.threadId);
    return () => held.release();
  });
  const aoNames: AoToolNames = {
    computer: (id) => {
      const entry = attachedBackendEntry(id);
      return entry ? backendDisplayName(entry) : remoteJobComputerName(item.threadId, id);
    },
    job: (requestId) => remoteJobRecord(item.threadId, requestId)?.label ?? '',
    page: (pageId) => {
      const page = browserCompanionState(item.threadId)?.pages?.find((p) => p.id === pageId);
      return page?.label || page?.title || '';
    },
    thread: (id) => {
      const thread = getThreadById(id);
      if (thread) return thread.title;
      // `thread_id` accepts an unambiguous prefix of at least six characters.
      if (id.length < 6 || id.length >= 36) return '';
      const matches = getThreads().filter((t) => t.id.startsWith(id));
      return matches.length === 1 ? matches[0].title : '';
    },
    run: (runId) => {
      const run = getWorkflowRun(runId);
      if (!run) return '';
      const goal = run.goal.trim().split('\n')[0];
      return [run.workflowId, goal].filter(Boolean).join(' · ');
    },
  };
  let aoTool = $derived(aoServer ? aoToolPresentation(displayMeta, aoNames) : null);
  // The wire projection clips a long input to its opening
  // (internal/itemwire), which is all the header shows. The body lists
  // inputs in full, so an expanded row reads them from the stored row.
  let aoInputElided = $derived.by(() => {
    const marker = displayMeta?.wireElision;
    if (!marker || typeof marker !== 'object' || Array.isArray(marker)) return false;
    const input = (marker as Record<string, unknown>).input;
    return Array.isArray(input) && input.length > 0;
  });
  let aoRecovered = $derived(
    aoInputElided ? readItemProjectionSource(item.threadId, effectiveDisplayItem.id, effectiveDisplayItem.updatedAt) : null,
  );
  let aoFactsMeta = $derived(
    aoRecovered?.source?.meta ? parseJsonObject(aoRecovered.source.meta) ?? displayMeta : displayMeta,
  );
  let aoInput = $derived.by<Record<string, unknown>>(() => {
    const input = aoFactsMeta?.input;
    return input && typeof input === 'object' && !Array.isArray(input) ? (input as Record<string, unknown>) : {};
  });
  let aoFacts = $derived(aoServer ? aoToolFacts(aoFactsMeta, aoNames) : []);
  let classification = $derived(
    aoTool
      ? { icon: aoTool.icon, label: aoTool.label, isSubagent: false }
      : classifyToolName(effectiveDisplayItem.toolName ?? effectiveDisplayItem.summary),
  );
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

  // A remote_run row is the job it started. While the call itself waits it
  // is a running tool call like any other; once the call has returned and
  // the job is still running on the far computer, the row reads as
  // backgrounded, with no timer, until the job's receipt settles it with
  // the job's own outcome and run time. The receipt is the thread's job
  // listing, which the watcher keeps current.
  let remoteJob = $derived(
    aoTool?.tool === 'remote_run' && aoTool.requestId && effectiveStatusItem.status === 'completed'
      ? remoteJobRecord(item.threadId, aoTool.requestId)
      : null,
  );
  let jobProjection = $derived.by<{
    status: Item['status'];
    isBackground: boolean;
    durationMs: number | null;
    error: { tone: 'error'; msg: string; code?: string } | null;
  } | null>(() => {
    const receipt = remoteJob?.receipt;
    if (!receipt?.id) return null;
    if (receipt.state === 'running') {
      return { status: 'running', isBackground: true, durationMs: null, error: null };
    }
    const finishedAt = receipt.finishedAt ?? 0;
    const durationMs = receipt.startedAt > 0 && finishedAt >= receipt.startedAt
      ? finishedAt - receipt.startedAt
      : null;
    const code = receipt.exitCode >= 0 ? `exit ${receipt.exitCode}` : undefined;
    switch (receipt.state) {
      case 'succeeded':
        return { status: 'completed', isBackground: false, durationMs, error: null };
      case 'canceled':
        return { status: 'killed', isBackground: false, durationMs, error: { tone: 'error', msg: 'Remote job stopped' } };
      default:
        return {
          status: 'errored',
          isBackground: false,
          durationMs,
          error: { tone: 'error', msg: receipt.error || `Remote job ${receipt.state}`, code },
        };
    }
  });
  let projectedStatusItem = $derived<Item>(
    jobProjection
      ? { ...effectiveStatusItem, status: jobProjection.status, isBackground: jobProjection.isBackground }
      : effectiveStatusItem,
  );

  let time = $derived(formatTimeOfDay(effectiveStatusItem.createdAt));

  let preview = $derived(
    presentToolCardInputPreview(effectiveDisplayItem, summaryMeta, displayMeta, paneWorkspacePath(pane), aoNames),
  );
  let previewClass = $derived(
    preview.path
      ? 'min-w-0 flex-1 whitespace-normal break-all text-[0.75rem] leading-4 text-fg-muted/75'
      : 'min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted/75',
  );

  let durationMs = $derived.by<number | null>(() => {
    if (jobProjection) return jobProjection.durationMs;
    if (!summaryMeta) return null;
    const d = summaryMeta.durationMs;
    if (typeof d === 'number' && d >= 0) return d;
    return null;
  });

  let isBackgroundedLaunch = $derived(
    projectedStatusItem.kind === 'tool_call' && projectedStatusItem.isBackground === true,
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

  let indicatorState = $derived(indicatorStateForItem(projectedStatusItem, { payloadMeta: statusMeta }));
  // Claude's SendMessage ack, stamped by triage as `send_reply`: the one
  // line the CLI's own TUI prints under the call ("Message queued for
  // …", "No agent named …"). Red as the row error when the CLI refused
  // the send, muted under the header otherwise.
  let sendReply = $derived.by(() => {
    if (item.toolName !== 'SendMessage') return '';
    const reply = itemMeta?.send_reply;
    return typeof reply === 'string' ? reply.trim() : '';
  });
  let rowError = $derived(
    jobProjection
      ? jobProjection.error
      : rowErrorWithFallback(effectiveStatusItem, {
        meta: statusMeta,
        fallback: sendReply || 'Tool call failed',
      }),
  );
  // An AO tool call that failed: its reply is the server's message, with a
  // `[code]` prefix on the thread and workflow servers. The payload's
  // preview carries it, so the row says what went wrong without a read.
  let aoRowError = $derived.by(() => {
    if (!aoTool || !rowError || rowError.code) return rowError;
    const preview = statusMeta?.preview;
    if (typeof preview !== 'string' || !preview.trim()) return rowError;
    const error = aoToolError(preview);
    return { tone: rowError.tone, msg: error.message, code: error.code || undefined };
  });

  // A spawn, send or ask whose call returned before the answer: where the
  // request stands now. The answer's arrival is the wake row carrying its
  // token; until then, what the target thread is doing.
  let aoAnswered = $derived(
    aoTool?.server === THREAD_TOOLS_SERVER
      && aoTool.result.token !== ''
      && answeredRequestTokens(pane?.items).has(aoTool.result.token),
  );
  let aoLive = $derived.by(() => {
    if (!aoTool || aoTool.server !== THREAD_TOOLS_SERVER) return '';
    if (!['thread_spawn', 'thread_send', 'thread_ask'].includes(aoTool.tool)) return '';
    const { token, threadId, state, outcome } = aoTool.result;
    if (!token || outcome === 'settled' || isSettledRequestState(state)) return '';
    if (aoAnswered) return 'answer arrived';
    // An ask runs in a hidden copy; the thread asked is not doing the work.
    if (aoTool.tool === 'thread_ask') return 'waiting on the answer';
    const target = threadId ? getThreadById(threadId) : undefined;
    if (!target) return '';
    const status = getEffectiveThreadStatus(target);
    switch (status) {
      case 'pending-approval':
        return 'waiting on an approval';
      case 'awaiting-input':
        return 'waiting on an answer from you';
      default:
        return `thread ${status.replace(/-/g, ' ')}`;
    }
  });


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
  let hasPayloadBody = $derived(
    Boolean(item.payloadId) ||
      deferredOutputState === 'loading' ||
      deferredOutputState === 'error',
  );
  // An AO tool row opens whenever it has more to say than its header: the
  // text the header clipped, the inputs it left out, or the result.
  let hasAoBody = $derived(
    aoTool !== null && (aoTool.fullWhat !== aoTool.what || aoFacts.length > 0),
  );
  let hasExpandableBody = $derived(!suppressBodyExpansion && (hasPayloadBody || hasAoBody));

  keepExpandedPayloadFresh(
    () => expansion,
    () => Boolean(item.payloadId),
  );

  async function toggle() {
    await expansion.toggle();
  }

  $effect(() => {
    if (expansion.expanded && aoTool && aoInputElided) {
      requestItemProjectionSource(item.threadId, effectiveDisplayItem.id, effectiveDisplayItem.updatedAt);
    }
  });

  // Where an AO tool acts, when that is not simply "here". A remote tool
  // names the computer its input targets: by name when this client or the
  // thread's job listing knows it, else by a short id, so two computers
  // still read apart. A browser tool drives a real page on the machine the
  // agent runs on: on the owner's own screen that page is the companion
  // browser and there is nothing to say, but read anywhere else the row is
  // the only sign the page exists at all, so it names the machine it is on.
  let where = $derived.by(() => {
    if (!aoTool) return { name: '', title: '' };
    if (aoTool.computerId) {
      const name = aoTool.computerName || aoComputerName(aoTool.computerId, aoNames);
      const title = aoTool.computerName
        ? `On ${aoTool.computerName}`
        : `On computer ${aoTool.computerId}, which this device is not paired with`;
      return { name, title };
    }
    if (aoTool.server === BROWSER_TOOLS_SERVER) {
      const threadId = pane?.threadId ?? '';
      if (!threadId || !previewRouted(threadId)) return { name: '', title: '' };
      const entry = attachedBackendEntry(threadMachine(threadId, null));
      const name = entry ? backendDisplayName(entry) : 'that computer';
      return { name, title: `Browsing on ${name}. The page is only visible there.` };
    }
    return { name: '', title: '' };
  });
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
      <ToolRowStatusIndicator item={projectedStatusItem} state={indicatorState} testId="tool-call-card-status" />
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
    {#snippet icon()}<ToolKindIcon kind={classification.icon} ariaLabel={aoTool ? aoTool.tool : classification.label} />{/snippet}
    {#snippet label()}<span data-testid="tool-call-card-label" title={aoTool?.tool}>{classification.label}</span>{/snippet}
    {#snippet body()}
      <span class={previewClass} data-testid="tool-call-card-preview">
        {#if preview.path}
          <EditorLink backend={threadMachine(item.threadId, pane?.thread?.projectId)}
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
          {preview.text}{#if aoLive}<!-- Inside the clipped text, after it: the hint is what truncation cuts first. --><span
              class="ml-1.5 text-fg-hint"
              data-testid="tool-call-card-ao-live-hint"
            >({aoLive})</span>{/if}
        {/if}
      </span>
      {#if where.name}
        <span
          class="ml-1.5 max-w-[45%] shrink-0 truncate text-[0.75rem] text-fg-hint"
          title={where.title}
          data-testid="tool-call-card-where"
          data-machine={where.name}
        >({where.name})</span>
      {/if}
    {/snippet}
    {#snippet actions()}
      {@render headerActions()}
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if aoRowError}
    <div class="ml-[5.25rem] compact:ml-5 px-3 pb-1">
      <RowError tone={aoRowError.tone} msg={aoRowError.msg} code={aoRowError.code} />
    </div>
  {:else if sendReply}
    <div
      class="ml-[5.25rem] px-3 pb-1 text-[0.75rem] text-fg-muted/75 break-words"
      data-testid="tool-call-card-reply"
    >{sendReply}</div>
  {/if}

  {#if hasExpandableBody && expansion.expanded}
    {#if aoTool}
      <AoToolBody
        {pane}
        {expansion}
        id={bodyDomId}
        tool={aoTool}
        facts={aoFacts}
        input={aoInput}
        names={aoNames}
        viewer={threadMachine(item.threadId, pane?.thread?.projectId)}
        failed={effectiveStatusItem.status === 'errored'}
        live={aoLive}
        answered={aoAnswered}
        errorShown={aoRowError?.msg ?? ''}
        hasPayload={hasPayloadBody}
        emptyMessage={importUnavailableLabel(item) ?? 'No stored payload for this tool result.'}
        {deferredOutputState}
        {deferredOutputError}
      />
    {:else}
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
  {/if}
</div>
