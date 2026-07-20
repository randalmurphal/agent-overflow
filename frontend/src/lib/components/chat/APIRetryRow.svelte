<script lang="ts">
  /*
   * Inline timeline row for transient provider retries. Rendered for
   * `EventAPIRetry` envelopes from either provider (Claude
   * `system.api_retry`, Codex `error+willRetry:true`) once the attempt
   * crosses the visibility threshold (handled in
   * internal/triage/api_retry.go — first three attempts are dropped
   * silently). The row updates in place as more retry attempts land
   * (deterministic id `retry:<turnIndex>`); `status` flips to
   * `completed` when triage observes a forward-progress event for the
   * thread, at which point the row reads as a static historical record
   * of "we paused, then kept going."
   *
   * Mirrors Claude Code's `SystemAPIErrorMessage.tsx` row UX:
   *   ⟳ Retrying (4/10, rate_limit)
   * with the icon pulsing while running and steady once completed.
   */
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';

  let { item }: { item: Item } = $props();

  const isLive = $derived(item.status === 'running' || item.status === 'streaming');
  const meta = $derived(parseJsonObject(item.meta));
  const attempt = $derived(typeof meta?.attempt === 'number' ? meta.attempt : 0);
  const maxRetries = $derived(typeof meta?.max_retries === 'number' ? meta.max_retries : 0);
  const errorReason = $derived(typeof meta?.error === 'string' ? meta.error : '');

  // Cap the tooltip body so a misbehaving provider can't render a
  // multi-screen native tooltip from a long error string. The HTML
  // title attribute is autoescaped by Svelte; the cap is purely a
  // layout/UX guard, not an injection mitigation.
  const MAX_TOOLTIP_REASON_CHARS = 200;
  const tooltip = $derived.by(() => {
    if (!errorReason) return '';
    const reason =
      errorReason.length > MAX_TOOLTIP_REASON_CHARS
        ? errorReason.slice(0, MAX_TOOLTIP_REASON_CHARS) + '...'
        : errorReason;
    if (attempt > 0 && maxRetries > 0) {
      return `Attempt ${attempt} of ${maxRetries}: ${reason}`;
    }
    return reason;
  });
</script>

<div
  class="mb-1.5 flex items-center gap-1.5 px-2 py-1 text-[0.6875rem] italic text-fg-subtle"
  data-testid="api-retry-row"
  data-status={item.status}
  title={tooltip}
  role={isLive ? 'status' : undefined}
  aria-live={isLive ? 'polite' : undefined}
>
  <Icon
    icon={RefreshCw}
    size={11}
    strokeWidth={2}
    class={isLive ? 'animate-pulse' : 'opacity-60'}
  />
  <span>{item.summary || 'Retrying provider request...'}</span>
</div>
