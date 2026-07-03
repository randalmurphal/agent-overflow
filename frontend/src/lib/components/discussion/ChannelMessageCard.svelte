<script lang="ts">
  // One channel-message card. Shared by ChannelView's settled-message list
  // and its in-progress live-tail card (the current speaker's streaming
  // text) — the two differ only in which props they pass: a settled
  // message has a sequence + timestamp and `streaming=false`; the live
  // tail has neither and passes `streaming=true` so ChatMarkdown treats
  // its content as a volatile, still-growing tail (see
  // components/chat/ChatMarkdown.svelte's `streaming` prop).
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import type { PathRef } from '../../types/models';

  let {
    workspacePath,
    accentClass,
    badgeClass,
    roleLabel,
    sequenceLabel,
    timestampLabel,
    content,
    pathRefs = [],
    streaming = false,
  }: {
    workspacePath: string;
    /** Card border/background classes — human/agent/system/live-tail
     * each get a distinct treatment (see ChannelView's `messageAccentClass`). */
    accentClass: string;
    /** Role-badge border/text/background classes. */
    badgeClass: string;
    roleLabel: string;
    /** Omit for the live-tail card — it has no sequence yet. */
    sequenceLabel?: string;
    /** Omit for the live-tail card — it has no createdAt yet. */
    timestampLabel?: string;
    content: string;
    pathRefs?: PathRef[];
    streaming?: boolean;
  } = $props();
</script>

<div class="rounded-[var(--radius-card)] border {accentClass} px-3.5 py-2.5">
  <div class="flex items-center gap-2 mb-1.5">
    <span
      class="rounded-[var(--radius-field)] border px-1.5 py-0.5 text-[0.625rem] font-semibold uppercase tracking-[0.14em] {badgeClass}"
    >
      {roleLabel}
    </span>
    {#if sequenceLabel}
      <span class="text-[0.6875rem] text-fg-hint tabular-nums">{sequenceLabel}</span>
    {/if}
    {#if timestampLabel}
      <span class="ml-auto text-[0.6875rem] text-fg-hint">{timestampLabel}</span>
    {/if}
  </div>
  <ChatMarkdown
    source={content}
    {workspacePath}
    {pathRefs}
    {streaming}
    class="text-[0.8125rem] text-fg break-words"
  />
</div>
