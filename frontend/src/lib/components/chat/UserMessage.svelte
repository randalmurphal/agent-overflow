<script lang="ts">
  import type { Item } from '../../types/models';

  let { item }: { item: Item } = $props();
</script>

<div class="flex justify-end mb-3">
  <div class="max-w-[80%] rounded-lg px-4 py-2.5 bg-accent/20 text-text-primary">
    <p class="whitespace-pre-wrap text-sm leading-relaxed">{item.summary}</p>
  </div>
</div>
