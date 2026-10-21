<script lang="ts">
  // The Pull requests tool pane (docs/specs/pull-requests.md, "The page").
  // The list and the review beside it are not built yet; this body holds the
  // pane's place in the strip with its header and an empty state.
  import ToolPaneHeader from '../panes/ToolPaneHeader.svelte';

  interface Props {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { paneId, onPaneDragStart }: Props = $props();
</script>

<ToolPaneHeader {paneId} kind="pulls" {onPaneDragStart} />
<div class="flex flex-1 items-center justify-center px-6" data-testid="pulls-pane-empty">
  <p class="text-sm text-fg-muted">No pull requests to show yet.</p>
</div>
