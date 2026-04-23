<script lang="ts">
  import Bell from 'lucide-svelte/icons/bell';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';

  let { item }: { item: Item } = $props();
</script>

<div
  class="mb-1.5 flex items-center gap-1.5 px-2 py-1 text-[11px] italic text-fg-subtle"
  data-testid="notification-row"
>
  <Icon icon={Bell} size={11} strokeWidth={2} class="opacity-70 shrink-0" />
  <span>{item.summary || 'Provider notification'}</span>
</div>
