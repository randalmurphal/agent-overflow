<script lang="ts">
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  let { title }: { title: string } = $props();
</script>

<div
  role="status"
  aria-busy="true"
  data-testid="fork-pending-row"
  class="flex min-w-0 items-center gap-2 rounded-[var(--radius-field)] px-2 py-2 text-xs text-fg-muted opacity-60 cursor-wait"
>
  <SteppedSpinner />
  <span class="truncate">{title}</span>
  <span class="ml-auto shrink-0">Forking…</span>
</div>
