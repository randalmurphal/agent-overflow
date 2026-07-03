<script lang="ts">
  // ChannelView's status bar: the Live/Concluded/Loading pill, message
  // count, turn counter, current-speaker label, and load-error banner.
  // Pure presentation — all state is derived by ChannelView and passed
  // down as props.
  let {
    concluded,
    statusLabel,
    messageCount,
    status,
    turnCount,
    maxTurns,
    awaitingResponse,
    currentSpeakerRole,
    loadError,
  }: {
    concluded: boolean;
    statusLabel: string;
    messageCount: number;
    status: string | null;
    turnCount: number;
    maxTurns: number;
    awaitingResponse: boolean;
    currentSpeakerRole: string;
    loadError: string | null;
  } = $props();
</script>

<div class="border-b border-border-subtle px-5 py-2 flex items-center gap-3 shrink-0">
  <span class="inline-flex items-center gap-1.5 rounded-[var(--radius-field)] border px-2 py-0.5 text-[0.625rem] font-medium uppercase tracking-wide
    {concluded ? 'border-border-subtle bg-surface-2/40 text-fg-muted' : 'border-success/30 bg-success/10 text-success'}">
    <span class="w-1.5 h-1.5 rounded-full {concluded ? 'bg-fg-subtle' : 'bg-success'}" aria-hidden="true"></span>
    {statusLabel}
  </span>
  <span class="text-[0.6875rem] text-fg-muted tabular-nums">
    {messageCount} {messageCount === 1 ? 'message' : 'messages'}
  </span>
  {#if status !== null}
    <span class="text-[0.6875rem] text-fg-muted tabular-nums">Turn {turnCount} of {maxTurns}</span>
  {/if}
  {#if awaitingResponse && status === 'open'}
    <span class="text-[0.6875rem] text-fg-subtle italic truncate max-w-[200px]">
      Speaking: {currentSpeakerRole}
    </span>
  {/if}
  {#if loadError}
    <span role="alert" class="ml-auto text-[0.6875rem] text-error truncate max-w-[280px]" title={loadError}>
      Error: {loadError}
    </span>
  {/if}
</div>
