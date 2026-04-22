<script lang="ts">
  import Drawer from '../Drawer.svelte';

  let {
    position = 'bottom' as 'bottom' | 'right',
    size = $bindable(320),
    minSize = 120,
    maxSize = undefined as number | undefined,
    resizable = true,
    extraClass = '',
    onResize = undefined as ((size: number) => void) | undefined,
  }: {
    position?: 'bottom' | 'right';
    size?: number;
    minSize?: number;
    maxSize?: number;
    resizable?: boolean;
    extraClass?: string;
    onResize?: (size: number) => void;
  } = $props();
</script>

<Drawer {position} bind:size {minSize} {maxSize} {resizable} class={extraClass} {onResize}>
  {#snippet children()}<div data-testid="drawer-body">Body</div>{/snippet}
</Drawer>
