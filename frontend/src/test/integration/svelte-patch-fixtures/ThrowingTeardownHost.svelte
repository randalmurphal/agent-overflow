<script lang="ts">
  import ThrowingTeardownItem from './ThrowingTeardownItem.svelte';

  interface Props {
    getItems: () => number[];
    readDep: () => number;
    throwOn: () => number[];
    onteardown: (item: number) => void;
  }
  let { getItems, readDep, throwOn, onteardown }: Props = $props();
</script>

{#each getItems() as item (item)}
  <ThrowingTeardownItem {item} {readDep} {throwOn} {onteardown} />
{/each}
