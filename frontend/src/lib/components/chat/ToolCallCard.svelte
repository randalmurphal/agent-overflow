<script lang="ts">
  // ToolCallCard dispatches a `tool_call` / `tool_completion` item to the
  // correct renderer. Two switches:
  //
  //   1. Payload-kind switch. If the item carries a structured payload
  //      (`proposed_plan`, `diff`, `command_output`, `tool_result`), we
  //      hand off to the existing specialized component which renders its
  //      own per-kind header + chevron body. These already ship with the
  //      right visual weight.
  //
  //   2. Per-tool-kind header switch for the generic fallback. When a tool
  //      call has no structured payload — the common case for Bash while
  //      running, for Grep, for MCP tools, etc. — we render a header row
  //      tagged with a tool-kind icon + label + decision chip + status,
  //      and defer the expandable body to ToolResultDropdown's existing
  //      GetPayloadPreview flow.
  //
  // Child subagent recursion stays in MessageTimeline's renderNode snippet
  // (passed through SubagentGroup). ToolCallCard does not take a children
  // prop in the current wiring; subagent grouping is handled upstream by
  // `groupItemsBySubagent` before we see the item.
  //
  // Spec: docs/architecture/chat-rewrite.md §ToolCallCard.

  import { slide } from 'svelte/transition';
  import type {
    CommandOutputMeta,
    DiffMeta,
    Item,
    ProposedPlanMeta,
    ToolResultMeta,
  } from '../../types/models';
  import CommandOutput from './CommandOutput.svelte';
  import DiffPreview from './DiffPreview.svelte';
  import ProposedPlanCard from './ProposedPlanCard.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import ToolResultCard from './ToolResultCard.svelte';
  import { classifyToolName } from './toolCardHeader';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';

  let { pane, item }: { pane: ThreadPane; item: Item } = $props();

  function parseMeta<T>(raw: string | undefined): T | null {
    if (!raw) return null;
    try {
      return JSON.parse(raw) as T;
    } catch {
      return null;
    }
  }

  // Structured-payload routing. `payloadKind` + a present payloadId are
  // required — a missing payloadId means the provider hasn't finished
  // emitting the content yet, and the generic header renders until it does.
  let payloadKind = $derived(item.payloadKind);
  let payloadId = $derived(item.payloadId);

  let planMeta = $derived<ProposedPlanMeta | null>(
    payloadKind === 'proposed_plan' && payloadId ? parseMeta<ProposedPlanMeta>(item.payloadMeta) : null,
  );
  let diffMeta = $derived<DiffMeta | null>(
    payloadKind === 'diff' && payloadId ? parseMeta<DiffMeta>(item.payloadMeta) : null,
  );
  let cmdMeta = $derived<CommandOutputMeta | null>(
    payloadKind === 'command_output' && payloadId ? parseMeta<CommandOutputMeta>(item.payloadMeta) : null,
  );
  let toolResultMeta = $derived<ToolResultMeta | null>(
    payloadKind === 'tool_result' && payloadId ? parseMeta<ToolResultMeta>(item.payloadMeta) : null,
  );

  // Generic-header fallback: when none of the structured-payload paths fire,
  // we render the tool-kind header ourselves and delegate the expandable
  // body to the same GetPayloadPreview flow that ToolResultDropdown uses.
  let classification = $derived(classifyToolName(item.toolName ?? item.summary));

  const expansion = createPayloadExpansion(() => item.payloadId);

  $effect(() => {
    item.id;
    item.payloadId;
    expansion.reset();
  });

  // Status/exit/duration parsing for the generic header. These come out of
  // the same payloadMeta dumping ground ToolResultDropdown used to read.
  let summaryMeta = $derived.by<Record<string, unknown> | null>(() => {
    const raw = item.payloadMeta;
    if (!raw) return null;
    try {
      const parsed = JSON.parse(raw) as unknown;
      if (parsed && typeof parsed === 'object') return parsed as Record<string, unknown>;
      return null;
    } catch {
      return null;
    }
  });

  let inputPreview = $derived.by<string>(() => {
    // Prefer the provider-supplied short summary. Fall back to the metadata
    // title (populated by server-side helpers) so Bash shows its command
    // even when the provider didn't set summary.
    const fromSummary = (item.summary ?? '').trim();
    if (fromSummary) return fromSummary;
    if (summaryMeta) {
      const title = summaryMeta.title;
      if (typeof title === 'string' && title.trim()) return title.trim();
    }
    return classification.displayName;
  });

  let exitCode = $derived.by<number | null>(() => {
    if (!summaryMeta) return null;
    const code = summaryMeta.exitCode;
    return typeof code === 'number' ? code : null;
  });

  let durationMs = $derived.by<number | null>(() => {
    if (!summaryMeta) return null;
    const d = summaryMeta.durationMs;
    if (typeof d === 'number' && d >= 0) return d;
    return null;
  });

  let statusLabel = $derived.by(() => {
    if (item.status === 'running' || item.status === 'streaming') return 'running';
    if (item.status === 'errored') return 'failed';
    return 'done';
  });

  let statusClass = $derived.by(() => {
    if (item.status === 'running' || item.status === 'streaming') return 'text-accent';
    if (item.status === 'errored') return 'text-error';
    return 'text-success';
  });

  let exitBadgeClass = $derived.by(() => {
    if (exitCode === null) return '';
    return exitCode === 0 ? 'bg-success/20 text-success' : 'bg-error/20 text-error';
  });

  function formatDuration(ms: number): string {
    if (ms < 1000) return `${ms}ms`;
    const seconds = ms / 1000;
    if (seconds < 60) return `${seconds.toFixed(1)}s`;
    const minutes = Math.floor(seconds / 60);
    const remSec = Math.round(seconds - minutes * 60);
    return `${minutes}m ${remSec}s`;
  }

  async function toggle() {
    await expansion.toggle();
  }

  function handleKeydown(evt: KeyboardEvent) {
    if (evt.key === 'Enter' || evt.key === ' ') {
      evt.preventDefault();
      toggle();
    }
  }
</script>

{#if planMeta && payloadId}
  <ProposedPlanCard {pane} {payloadId} meta={planMeta} />
{:else if diffMeta && payloadId}
  <DiffPreview {item} meta={diffMeta} {payloadId} />
{:else if cmdMeta && payloadId}
  <CommandOutput {item} meta={cmdMeta} {payloadId} />
{:else if toolResultMeta && payloadId}
  <!-- File-change / command-mutation helpers attach a tool_result payload
       to the lifecycle row; render the rich diff card so file edits keep
       their existing visual weight. Gating on payloadKind (not just a
       successful JSON parse) avoids tool_call_result payloads coincidentally
       matching the ToolResultMeta shape and rendering as an empty card. -->
  <ToolResultCard {item} meta={toolResultMeta} {payloadId} />
{:else}
  <!-- Generic fallback: per-tool-kind header + chevron body. Matches the
       pre-extraction ToolResultDropdown visually but with a tool-kind
       icon on the left and the classifier-derived label. -->
  <div
    class="mb-2 overflow-hidden rounded border border-border bg-surface-1"
    data-testid="tool-call-card"
    data-tool-kind={classification.icon}
  >
    <button
      type="button"
      class="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      onclick={toggle}
      onkeydown={handleKeydown}
      aria-expanded={expansion.expanded}
      aria-controls="tool-call-card-body-{item.id}"
      data-testid="tool-call-card-toggle"
    >
      <span class="text-xs text-text-secondary select-none" aria-hidden="true">{expansion.expanded ? '▼' : '▶'}</span>
      <ToolKindIcon kind={classification.icon} ariaLabel={classification.label} />
      <span class="text-xs font-medium text-text-secondary shrink-0" data-testid="tool-call-card-label">
        {classification.label}
      </span>
      {#if item.kind === 'tool_call' && item.isBackground && item.status === 'running'}
        <!-- Backgrounded-running badge. Visual signal that the launch row is
             legitimately still "running" because the agent dispatched this
             to the background and moved on — not that the tool is actively
             executing right now. See docs/architecture/turn-lifecycle.md
             §UI components driven by this state + invariants.md §24. -->
        <span
          class="shrink-0 inline-flex items-center rounded-full bg-accent/15 px-1.5 py-0.5 text-[10px] font-medium leading-none text-accent"
          title="Running in background"
          aria-label="Backgrounded"
          data-testid="tool-call-backgrounded-badge"
        >
          …
        </span>
      {/if}
      <span class="min-w-0 flex-1 truncate text-sm text-text-primary" data-testid="tool-call-card-preview">
        {inputPreview}
      </span>
      <ToolDecisionChip decision={item.decision} />
      <span
        class="shrink-0 text-xs {statusClass}"
        data-testid="tool-call-card-status"
        data-status={item.status}
      >
        {statusLabel}
      </span>
      {#if exitCode !== null}
        <span
          class="shrink-0 rounded-full px-1.5 py-0.5 text-xs {exitBadgeClass}"
          data-testid="tool-call-card-exit"
        >
          exit {exitCode}
        </span>
      {:else if durationMs !== null}
        <span
          class="shrink-0 tabular-nums text-xs text-text-secondary"
          data-testid="tool-call-card-duration"
        >
          {formatDuration(durationMs)}
        </span>
      {/if}
    </button>

    {#if expansion.expanded}
      <div
        id="tool-call-card-body-{item.id}"
        transition:slide={{ duration: 150 }}
        class="border-t border-border bg-surface-0"
        data-testid="tool-call-card-body"
      >
        {#if expansion.loading}
          <p
            class="px-3 py-2 text-xs text-text-secondary animate-pulse"
            role="status"
            aria-live="polite"
          >
            Loading…
          </p>
        {:else if expansion.error}
          <p class="px-3 py-2 text-xs text-error" role="alert">
            Failed to load: {expansion.error}
          </p>
        {:else if expansion.displayData !== null}
          <pre
            class="ansi-body max-h-60 overflow-auto whitespace-pre-wrap break-words px-3 py-2 font-mono text-xs leading-relaxed text-text-secondary"
            data-testid="tool-call-card-output"
          >{@html expansion.displayHtml ?? ''}</pre>
          {#if expansion.hasMore}
            <button
              type="button"
              class="mx-3 mb-3 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
              onclick={() => expansion.showFull()}
              data-testid="tool-call-card-show-full"
            >
              Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
            </button>
          {/if}
        {:else}
          <p class="px-3 py-2 text-xs text-text-secondary italic">
            No stored payload for this tool result.
          </p>
        {/if}
      </div>
    {/if}
  </div>
{/if}
