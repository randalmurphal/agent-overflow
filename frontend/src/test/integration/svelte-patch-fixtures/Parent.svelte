<script lang="ts">
  import Child from './Child.svelte';
  import ChildNoDefault from './ChildNoDefault.svelte';
  import ChildTemplateRead from './ChildTemplateRead.svelte';
  import type { FixtureItem, PaneLike } from './types';

  interface Props {
    getPane: () => PaneLike;
    getItems: () => FixtureItem[];
    onInit: (workspacePath: string) => void;
  }
  let { getPane, getItems, onInit }: Props = $props();

  let pane = $derived(getPane());
  function paneWorkspacePath(p: PaneLike): string {
    return p?.thread?.workspacePath ?? '';
  }
</script>

{#each getItems() as item (item.key)}
  {#if item.variant === 'default'}
    <Child workspacePath={paneWorkspacePath(pane)} title={item.title} {onInit} />
  {:else if item.variant === 'no-default'}
    <ChildNoDefault workspacePath={paneWorkspacePath(pane)} title={item.title} {onInit} />
  {:else}
    <ChildTemplateRead workspacePath={paneWorkspacePath(pane)} title={item.title} {onInit} />
  {/if}
{/each}
