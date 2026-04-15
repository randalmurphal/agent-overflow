<script lang="ts">
  import type { Item } from '../types/models';
  import Markdown from './Markdown.svelte';

  let { item }: { item: Item } = $props();
</script>

<div class="flex justify-start mb-3">
  <div class="max-w-[85%] rounded-lg px-4 py-2.5 bg-surface-2 text-text-primary">
    <Markdown content={item.summary} />
  </div>
</div>
