<script lang="ts">
  import type { Item } from '../../types/models';

  let { item }: { item: Item } = $props();

  // Display-only short time. Purely cosmetic — the authoritative
  // timestamps for turn accounting live on the pane, not on user text.
  const time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );
</script>

<div class="group flex justify-end mb-5">
  <div class="flex max-w-[82%] flex-col items-end gap-1">
    <div
      class="rounded-[18px] rounded-br-[8px] border border-border-subtle bg-surface-2/60
             px-3.5 py-2 text-[13px] leading-[1.55] text-fg shadow-sheet"
    >
      <p class="whitespace-pre-wrap">{item.summary}</p>
    </div>
    <time
      class="text-[10px] text-fg-hint opacity-0 transition-opacity duration-150 group-hover:opacity-100"
      datetime={new Date(item.createdAt).toISOString()}
    >
      {time}
    </time>
  </div>
</div>
