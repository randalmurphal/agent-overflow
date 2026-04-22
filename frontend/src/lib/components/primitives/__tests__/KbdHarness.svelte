<!--
  Harness for <Kbd>. The children snippet is emitted explicitly so
  Svelte 5 doesn't flag a snippet_conflict with implicit content.
-->
<script lang="ts">
  import Kbd from '../Kbd.svelte';

  let {
    label = '⌘K',
    extraClass = '',
  }: {
    label?: string;
    extraClass?: string;
  } = $props();
</script>

<Kbd class={extraClass}>
  {#snippet children()}{label}{/snippet}
</Kbd>
