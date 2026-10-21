<script lang="ts">
  import ToolPaneHeader from '../../lib/components/panes/ToolPaneHeader.svelte';

  let { paneId, onPaneDragStart }: { paneId: string; onPaneDragStart?: (event: DragEvent) => void } = $props();
</script>

<ToolPaneHeader {paneId} kind="workflows" {onPaneDragStart} />
<div data-testid="stub-tool-body" data-pane-id={paneId}>Tool body</div>
