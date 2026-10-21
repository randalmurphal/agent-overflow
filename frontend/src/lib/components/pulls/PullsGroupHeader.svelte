<script lang="ts">
  import MicroLabel from '../primitives/MicroLabel.svelte';

  // A group heading in the page's lists ("Needs you · 3", a project's name
  // over its rows or its error). Sticky so the group a reader is in stays
  // named while its rows scroll.

  interface Props {
    label: string;
    /** The count on the right; omitted for a group with no rows to count. */
    count?: number | null;
    testid?: string;
  }

  let { label, count = null, testid }: Props = $props();
</script>

<div
  class="sticky top-0 z-[1] flex items-center justify-between border-b border-border-subtle bg-surface-0/95 px-3 py-1.5 backdrop-blur-sm"
  data-testid={testid}
>
  <MicroLabel as="h3">{label}</MicroLabel>
  {#if count !== null}
    <span class="text-[0.6875rem] tabular-nums text-fg-subtle">{count}</span>
  {/if}
</div>
