<!-- Fixture for svelte-patch-flip-phases.test.ts: an animated keyed-each
     paired with an $effect that triggers a second reorder of the SAME list
     inside the same flush, exercising cross-batch FLIP deduplication. -->
<script lang="ts">
  let {
    getItems,
    onFirstReorder,
  }: {
    getItems: () => number[];
    onFirstReorder: () => void;
  } = $props();

  let armed = false;
  $effect(() => {
    const list = getItems();
    if (armed && list[0] === 4) {
      armed = false;
      onFirstReorder();
    } else if (list[0] === 1) {
      armed = true;
    }
  });

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
