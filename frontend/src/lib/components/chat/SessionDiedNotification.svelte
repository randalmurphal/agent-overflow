<script lang="ts">
  /*
   * Notification renderer for the `session_died` discriminator. The
   * row is the historical trace of a provider session terminating
   * mid-turn (process exit, kernel-killed, EOF). The matching
   * `provider:session_died` event drives the top reconnect banner
   * separately — these are loosely-coupled siblings, not one bound to
   * the other (see internal/triage/session_status.go).
   *
   * Decoded meta shape (sanitizedTimelineNotificationMeta in
   * internal/triage/timeline_notifications.go):
   *   { kind: "session_died", title, reason?, exitCode?, signal?, stderrTail? }
   */
  import PowerOff from '@lucide/svelte/icons/power-off';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';

  let { item }: { item: Item } = $props();

  const meta = $derived(parseJsonObject(item.meta));
  const reason = $derived(typeof meta?.reason === 'string' ? meta.reason : '');
  const exitCode = $derived(typeof meta?.exitCode === 'number' ? meta.exitCode : 0);
  const signal = $derived(typeof meta?.signal === 'string' ? meta.signal : '');
  // Backend-sanitized last stderr output — the actual failure text when
  // the process died without wire output (bad CLI flag, missing module).
  const stderrTail = $derived(typeof meta?.stderrTail === 'string' ? meta.stderrTail : '');

  // Prefer the most specific signal/exit-code we have. Reason is the
  // wire's pre-baked description; surface it as a tooltip even when we
  // render a tighter line ourselves.
  const detail = $derived.by(() => {
    if (signal) return `Killed by signal ${signal}`;
    if (exitCode) return `Exited with code ${exitCode}`;
    return '';
  });
</script>

<div
  class="mb-1.5 flex items-start gap-1.5 px-2 py-1 text-[0.6875rem] italic text-fg-subtle"
  data-testid="session-died-notification"
  title={reason || undefined}
>
  <Icon icon={PowerOff} size={11} strokeWidth={2} class="mt-0.5 shrink-0 opacity-70" />
  <div class="flex-1">
    <div>{item.summary || 'Provider session ended'}</div>
    {#if detail}
      <div class="not-italic font-mono text-fg-hint">{detail}</div>
    {/if}
    {#if stderrTail}
      <div class="not-italic font-mono text-fg-hint break-all">{stderrTail}</div>
    {/if}
  </div>
</div>
