<script lang="ts">
  import { slide } from 'svelte/transition';
  import { GetPayloadData } from '../../stores/bindings';
  import type { Item, PayloadMeta } from '../../types/models';

  interface Props {
    item: Item;
    payloadMeta?: PayloadMeta;
  }

  let { item, payloadMeta }: Props = $props();

  /**
   * The Item fields we read (`status`) are being added to
   * `src/lib/types/models.ts` by another agent. Access defensively until
   * that type extension lands so this component compiles with the
   * current baseline.
   */
  type StatusFields = { status?: 'running' | 'completed' | 'failed' };
  const itemStatus = $derived((item as unknown as StatusFields).status ?? 'completed');

  // Local expansion + cache state. `fullBody` stays populated after the
  // first successful fetch so collapse + re-expand does NOT re-hit the
  // binding — matches the rule in frontend/CLAUDE.md that heavy content
  // is on-demand but cached per component instance.
  let expanded = $state(false);
  let loading = $state(false);
  let loadError = $state<string | null>(null);
  let fullBody = $state<string | null>(null);

  /**
   * payloadMeta.meta is stored as a JSON string so kind-specific data
   * stays opaque to the store layer. We pick out a couple of fields the
   * summary row cares about (exit code, duration, title) with defensive
   * parsing — a garbage string just yields a null lookup, not a crash.
   */
  const summaryMeta = $derived.by<Record<string, unknown> | null>(() => {
    const raw = payloadMeta?.meta;
    if (!raw) return null;
    try {
      const parsed = JSON.parse(raw) as unknown;
      if (parsed && typeof parsed === 'object') {
        return parsed as Record<string, unknown>;
      }
      return null;
    } catch {
      return null;
    }
  });

  const toolName = $derived.by<string>(() => {
    const fromSummary = (item.summary ?? '').trim();
    if (fromSummary) return fromSummary;
    if (summaryMeta) {
      const title = summaryMeta.title;
      if (typeof title === 'string' && title.trim()) return title.trim();
    }
    // Fall back to the item kind so the row never renders nameless.
    if (item.kind) return item.kind;
    return 'Tool';
  });

  const exitCode = $derived.by<number | null>(() => {
    if (!summaryMeta) return null;
    const code = summaryMeta.exitCode;
    return typeof code === 'number' ? code : null;
  });

  const durationMs = $derived.by<number | null>(() => {
    if (!summaryMeta) return null;
    const d = summaryMeta.durationMs;
    if (typeof d === 'number' && d >= 0) return d;
    return null;
  });

  const statusLabel = $derived.by(() => {
    if (itemStatus === 'running') return 'running';
    if (itemStatus === 'failed') return 'failed';
    return 'done';
  });

  const statusClass = $derived.by(() => {
    if (itemStatus === 'running') return 'text-accent';
    if (itemStatus === 'failed') return 'text-error';
    return 'text-success';
  });

  const exitBadgeClass = $derived.by(() => {
    if (exitCode === null) return '';
    return exitCode === 0
      ? 'bg-success/20 text-success'
      : 'bg-error/20 text-error';
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
    if (expanded) {
      expanded = false;
      return;
    }

    expanded = true;

    // Already cached — collapse/re-expand doesn't re-fetch.
    if (fullBody !== null) return;
    if (!item.payloadId) return;

    loading = true;
    loadError = null;
    try {
      fullBody = await GetPayloadData(item.payloadId);
    } catch (err) {
      loadError = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }

  function handleKeydown(evt: KeyboardEvent) {
    if (evt.key === 'Enter' || evt.key === ' ') {
      evt.preventDefault();
      toggle();
    }
  }
</script>

<div
  class="mb-2 overflow-hidden rounded border border-border bg-surface-1"
  data-testid="tool-result-dropdown"
>
  <button
    type="button"
    class="flex w-full items-center gap-2 px-3 py-2 text-left hover:bg-surface-2/40 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    onclick={toggle}
    onkeydown={handleKeydown}
    aria-expanded={expanded}
    aria-controls="tool-result-dropdown-body-{item.id}"
    data-testid="tool-result-dropdown-toggle"
  >
    <span class="text-xs text-text-secondary select-none" aria-hidden="true">{expanded ? '▼' : '▶'}</span>
    <span class="font-mono text-xs text-text-secondary shrink-0" aria-hidden="true">[T]</span>
    <span class="min-w-0 flex-1 truncate text-sm text-text-primary">{toolName}</span>
    <span
      class="shrink-0 text-xs {statusClass}"
      data-testid="tool-result-dropdown-status"
      data-status={itemStatus}
    >
      {statusLabel}
    </span>
    {#if exitCode !== null}
      <span
        class="shrink-0 rounded-full px-1.5 py-0.5 text-xs {exitBadgeClass}"
        data-testid="tool-result-dropdown-exit"
      >
        exit {exitCode}
      </span>
    {:else if durationMs !== null}
      <span
        class="shrink-0 tabular-nums text-xs text-text-secondary"
        data-testid="tool-result-dropdown-duration"
      >
        {formatDuration(durationMs)}
      </span>
    {/if}
  </button>

  {#if expanded}
    <div
      id="tool-result-dropdown-body-{item.id}"
      transition:slide={{ duration: 150 }}
      class="border-t border-border bg-surface-0"
      data-testid="tool-result-dropdown-body"
    >
      {#if loading}
        <p
          class="px-3 py-2 text-xs text-text-secondary animate-pulse"
          role="status"
          aria-live="polite"
          data-testid="tool-result-dropdown-loading"
        >
          Loading…
        </p>
      {:else if loadError}
        <p
          class="px-3 py-2 text-xs text-error"
          role="alert"
          data-testid="tool-result-dropdown-error"
        >
          Failed to load: {loadError}
        </p>
      {:else if fullBody !== null}
        <pre
          class="max-h-60 overflow-auto whitespace-pre-wrap break-words px-3 py-2 font-mono text-xs leading-relaxed text-text-secondary"
          data-testid="tool-result-dropdown-output"
        >{fullBody}</pre>
      {:else}
        <p
          class="px-3 py-2 text-xs text-text-secondary italic"
          data-testid="tool-result-dropdown-empty"
        >
          No stored payload for this tool result.
        </p>
      {/if}
    </div>
  {/if}
</div>
