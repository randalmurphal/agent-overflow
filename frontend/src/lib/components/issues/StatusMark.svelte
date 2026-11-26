<script lang="ts">
  // A status group's mark, in the convention most trackers share: an empty
  // ring to do, a half-filled ring in progress, a check done, a dashed ring
  // in the backlog, a struck ring canceled.
  interface Props {
    /** backlog, todo, in_progress, done or canceled: the group the
     * project puts the status in (`stores/statusMarkGroup.ts#markGroup`),
     * never a status's own group read directly. */
    group: string | undefined;
    size?: number;
  }

  let { group, size = 13 }: Props = $props();

  const tone = $derived(
    group === 'in_progress' ? 'text-warning' : group === 'done' ? 'text-success' : 'text-fg-muted',
  );
</script>

<svg class="shrink-0 {tone}" width={size} height={size} viewBox="0 0 16 16" aria-hidden="true" data-status-group={group}>
  {#if group === 'done'}
    <circle cx="8" cy="8" r="7" fill="currentColor" />
    <path d="m5 8.2 2 2 4-4.2" fill="none" class="stroke-surface-0" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
  {:else if group === 'in_progress'}
    <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="1.6" />
    <path d="M8 4.2a3.8 3.8 0 0 1 0 7.6z" fill="currentColor" />
  {:else if group === 'backlog'}
    <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="1.6" stroke-dasharray="2.4 2" />
  {:else if group === 'canceled'}
    <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="1.6" />
    <path d="m5.5 5.5 5 5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" />
  {:else}
    <circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="1.6" />
  {/if}
</svg>
