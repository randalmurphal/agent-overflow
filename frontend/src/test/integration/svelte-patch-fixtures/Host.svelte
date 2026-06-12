<script lang="ts">
  import Parent from './Parent.svelte';
  import type { FixtureItem, PaneLike } from './types';

  interface Props {
    getPane: () => PaneLike;
    getItems: () => FixtureItem[];
    isOpen: () => boolean;
    onInit: (workspacePath: string) => void;
  }
  let { getPane, getItems, isOpen, onInit }: Props = $props();
</script>

{#if isOpen()}
  <Parent {getPane} {getItems} {onInit} />
{/if}
