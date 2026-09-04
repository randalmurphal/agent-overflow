<!-- Fixture for svelte-patch-each-key-repair.test.ts: a keyed {#each} whose
     keys the test controls, so it can hand the block a repeat, plus a
     sibling text node driven by its own prop.

     The sibling sits AFTER the each block on purpose. Effects flush in tree
     order, so pristine svelte's `each_key_duplicate` throw aborts the batch
     before the sibling's text effect runs and the stale text stands — which
     is exactly the freeze this hunk exists to prevent, and the reason the
     sibling is the test's non-freeze proof. -->
<script lang="ts">
  import type { DuplicateKeyItem } from './types';

  interface Props {
    getItems: () => DuplicateKeyItem[];
    getLabel: () => string;
  }
  let { getItems, getLabel }: Props = $props();
</script>

<ul>
  {#each getItems() as item (item.key)}
    <li data-key={String(item.key)}>{item.label}</li>
  {/each}
</ul>
<p data-testid="sibling">{getLabel()}</p>
