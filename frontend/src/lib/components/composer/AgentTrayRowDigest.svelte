<script lang="ts">
  // The Agents row's digest: what its thread is doing now, what it is
  // blocked on in full, the last thing said there, and the answer once it
  // settled. Every field is on the row the listing read; nothing is fetched.
  // The Background row's digest (BackgroundTaskTrayDigest) shows a provider
  // subagent's transcript; a thread has a pane of its own for that.
  import type { ThreadAgentRow } from '../../stores/bindings';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';

  let { row, id }: { row: ThreadAgentRow; id: string } = $props();

  let lastSpeaker = $derived(row.lastMessageRole === 'user' ? 'Last asked' : 'Last said');
</script>

<div {id} class="ml-6 border-l border-border-subtle bg-surface-0/35" data-testid="agent-tray-row-digest">
  <dl class="grid max-h-48 grid-cols-[auto_1fr] gap-x-2 gap-y-1 overflow-auto px-3 py-2 text-[0.6875rem]" use:nestedScroll>
    {#if row.activity}
      <dt class="text-fg-hint">Doing</dt>
      <dd class="break-words text-fg-muted" data-testid="agent-tray-row-digest-activity">{row.activity}</dd>
    {/if}
    {#if row.blockedDetail}
      <dt class="text-fg-hint">Blocked on</dt>
      <dd class="whitespace-pre-wrap break-words text-fg-muted" data-testid="agent-tray-row-digest-blocked">{row.blockedDetail}</dd>
    {/if}
    {#if row.lastMessage}
      <dt class="text-fg-hint">{lastSpeaker}</dt>
      <dd class="whitespace-pre-wrap break-words text-fg-muted" data-testid="agent-tray-row-digest-last">{row.lastMessage}</dd>
    {/if}
    {#if row.answer}
      <dt class="text-fg-hint">Answer</dt>
      <dd class="whitespace-pre-wrap break-words text-fg-muted" data-testid="agent-tray-row-digest-answer">{row.answer}</dd>
    {/if}
    {#if row.nestedUnavailable}
      <dt class="text-fg-hint">Not shown</dt>
      <dd class="break-words text-warning" data-testid="agent-tray-row-digest-degraded">{row.nestedUnavailable}</dd>
    {/if}
  </dl>
</div>
