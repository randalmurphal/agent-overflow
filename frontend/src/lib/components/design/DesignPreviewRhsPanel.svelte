<script lang="ts">
  import type { PanelContext } from '../../stores/rhsPanelSlot.svelte';
  import DesignOptionsPanel from './DesignOptionsPanel.svelte';
  import DesignPreviewPanel from './DesignPreviewPanel.svelte';

  let { ctx }: { ctx: PanelContext } = $props();
</script>

{#if ctx.activeOptionSet}
  <DesignOptionsPanel {ctx} />
{:else}
  <DesignPreviewPanel {ctx} />
{/if}
