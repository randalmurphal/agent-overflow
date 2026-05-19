<script lang="ts">
  // Stub ChatView used by PaneHost.test.ts. The real ChatView mounts a
  // virtualizer + composer + ChatHeader, which would explode the test
  // surface. We only need a placeholder that exposes the pane's drag
  // handle so reorder integration tests can fire dragstart at a
  // realistic element.
  import type { ThreadPane } from '../../lib/stores/thread.svelte';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { pane, onPaneDragStart }: Props = $props();
</script>

<button
  type="button"
  data-testid="chat-header-title"
  data-pane-id={pane.paneId}
  draggable={onPaneDragStart != null}
  ondragstart={(event) => onPaneDragStart?.(event)}
>{pane.thread?.title ?? 'Pane'}</button>
