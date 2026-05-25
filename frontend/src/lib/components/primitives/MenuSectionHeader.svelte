<script lang="ts">
  // Small-caps label that groups a block of menu items. role="presentation"
  // so AT consumers skip it when moving between menuitems (the grouping is
  // purely visual — semantic grouping would require role="group" with
  // aria-labelledby, which we don't need for our current menus).

  interface Props {
    label: string;
  }

  let { label }: Props = $props();
</script>

<div
  role="presentation"
  data-menu-section-header
  class="px-3 pt-2 pb-1 text-[0.625rem] font-semibold uppercase tracking-[0.12em] text-text-secondary/70 select-none"
>
  {label}
</div>
