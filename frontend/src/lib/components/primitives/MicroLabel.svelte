<script lang="ts">
  /*
   * Uppercase micro-label. The "SECTION TITLE" voice used for Projects,
   * Plans, Tool Calls (N), and any group header that should register as
   * categorical structure without pulling focus.
   *
   * 11px × 0.18em tracking lands where t3-code's equivalent sits. The
   * previous codebase uses 0.22em which is too airy at this size and
   * makes the label feel like a logotype.
   */
  import type { Snippet } from 'svelte';

  interface Props {
    as?: 'h2' | 'h3' | 'h4' | 'span' | 'p';
    class?: string;
    children: Snippet;
  }

  let { as = 'span', class: className = '', children }: Props = $props();
</script>

<svelte:element
  this={as}
  class={[
    'text-[0.6875rem] font-medium uppercase tracking-[0.18em] text-fg-subtle',
    className,
  ].join(' ')}
>
  {@render children()}
</svelte:element>
