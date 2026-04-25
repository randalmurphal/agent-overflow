<script lang="ts">
  interface Props {
    expanded: boolean;
    count: number;
    anyRunning: boolean;
    canStopAll: boolean;
    stopAllInFlight: boolean;
    onToggle: () => void;
    onStopAll: () => void;
  }

  let {
    expanded,
    count,
    anyRunning,
    canStopAll,
    stopAllInFlight,
    onToggle,
    onStopAll,
  }: Props = $props();

  function headerKeydown(evt: KeyboardEvent) {
    if (evt.key === 'Enter' || evt.key === ' ') {
      evt.preventDefault();
      onToggle();
    }
  }
</script>

<div class="flex w-full items-center gap-2">
  <button
    type="button"
    class="flex flex-1 items-center gap-2 px-3 py-2 text-left hover:bg-surface-2/25 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors sm:px-4"
    onclick={onToggle}
    onkeydown={headerKeydown}
    aria-expanded={expanded}
    aria-controls="background-task-tray-body"
    data-testid="background-task-tray-header"
  >
    <span
      class="text-[11px] text-fg-subtle select-none transition-transform duration-150"
      class:rotate-90={expanded}
      aria-hidden="true"
    >▶</span>
    <span class="text-[9px] uppercase tracking-[0.16em] text-fg-subtle">Background</span>
    <span
      class="rounded-[var(--radius-field)] bg-accent/15 px-1 text-[10px] font-medium text-accent"
      data-testid="background-task-tray-count"
    >
      {count}
    </span>
    {#if anyRunning}
      <span
        class="h-1.5 w-1.5 rounded-full bg-accent animate-pulse"
        aria-hidden="true"
        data-testid="background-task-tray-pulse"
      ></span>
    {/if}
  </button>
  {#if canStopAll}
    <button
      type="button"
      class="mr-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/60 px-2 py-0.5 text-[11px] font-medium text-text-secondary hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors disabled:cursor-not-allowed disabled:opacity-50"
      onclick={onStopAll}
      disabled={stopAllInFlight}
      data-testid="background-task-tray-stop-all"
      aria-label="Stop All Running Background Tasks"
    >
      {stopAllInFlight ? 'Stopping…' : 'Stop All'}
    </button>
  {/if}
</div>
