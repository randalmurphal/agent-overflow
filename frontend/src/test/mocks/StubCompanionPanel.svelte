<script lang="ts">
  import type { PanelContext } from '../../lib/stores/panelContext.svelte';

  let { ctx }: { ctx: PanelContext } = $props();
</script>

<div data-testid="stub-companion-panel" data-pane-id={ctx.paneId}>
  Companion panel
</div>
