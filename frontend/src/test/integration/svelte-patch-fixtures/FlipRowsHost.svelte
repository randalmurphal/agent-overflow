<!-- Fixture for svelte-patch-flip-phases.test.ts: a keyed animated each
     whose animate fn is pure math (no style/layout reads), so every
     getBoundingClientRect the block performs comes from svelte's animation
     manager and the test can assert the phase order of the reorder pass. -->
<script lang="ts">
  let { getItems }: { getItems: () => number[] } = $props();

  function rowFlip(
    node: Element,
    { from, to }: { from: DOMRect; to: DOMRect },
  ) {
    void node;
    const dy = from.top - to.top;
    return {
      duration: 100,
      css: (_t: number, u: number) => `transform: translateY(${u * dy}px);`,
    };
  }
</script>

<ul>
  {#each getItems() as item (item)}
    <li animate:rowFlip data-id={item}>{item}</li>
  {/each}
</ul>
