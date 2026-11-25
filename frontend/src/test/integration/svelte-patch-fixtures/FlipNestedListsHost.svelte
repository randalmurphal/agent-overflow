<!-- Fixture for nested animated keyed-each blocks in svelte-patch-flip-phases.test.ts:
     mirrors ProjectList -> SidebarTreeList where outer groups and inner rows both
     animate:flip and update in the same reactive flush. -->
<script lang="ts">
  export interface NestedGroup {
    id: string;
    items: string[];
  }

  let {
    getGroups,
  }: {
    getGroups: () => NestedGroup[];
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

<div data-nested-root>
  {#each getGroups() as group (group.id)}
    <section animate:rowFlip data-id={group.id}>
      <ul>
        {#each group.items as item (item)}
          <li animate:rowFlip data-id={item}>{item}</li>
        {/each}
      </ul>
    </section>
  {/each}
</div>
