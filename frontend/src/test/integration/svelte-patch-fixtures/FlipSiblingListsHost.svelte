<!-- Fixture for cross-block FLIP phasing in svelte-patch-flip-phases.test.ts:
     two sibling keyed animated each blocks updated in the same reactive flush. -->
<script lang="ts">
  let {
    getItemsA,
    getItemsB,
  }: {
    getItemsA: () => string[];
    getItemsB: () => string[];
  } = $props();

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

<section data-list="a">
  <ul>
    {#each getItemsA() as item (item)}
      <li animate:rowFlip data-id={item}>{item}</li>
    {/each}
  </ul>
</section>
<section data-list="b">
  <ul>
    {#each getItemsB() as item (item)}
      <li animate:rowFlip data-id={item}>{item}</li>
    {/each}
  </ul>
</section>
