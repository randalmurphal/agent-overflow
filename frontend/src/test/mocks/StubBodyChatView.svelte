<script lang="ts">
  // Stub ChatView for PaneHost geometry tests that observe the pane body:
  // a title (the drag handle) above a body that fills the rest of the
  // section, the way the real ChatView's column does.
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
  draggable={onPaneDragStart != null}
  ondragstart={(event) => onPaneDragStart?.(event)}
>{pane.thread?.title ?? 'Pane'}</button>
<div class="min-h-0 flex-1 overflow-x-auto" data-testid="stub-pane-body" data-body-of={pane.paneId}>
  <div style="width:2000px;height:10px"></div>
</div>
