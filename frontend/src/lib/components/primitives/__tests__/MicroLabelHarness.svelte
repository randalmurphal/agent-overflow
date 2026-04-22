<script lang="ts">
  import MicroLabel from '../MicroLabel.svelte';

  let {
    label = 'Projects',
    as = 'span' as 'h2' | 'h3' | 'h4' | 'span' | 'p',
    extraClass = '',
  }: {
    label?: string;
    as?: 'h2' | 'h3' | 'h4' | 'span' | 'p';
    extraClass?: string;
  } = $props();
</script>

<MicroLabel {as} class={extraClass}>
  {#snippet children()}{label}{/snippet}
</MicroLabel>
