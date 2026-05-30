<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import LazyThreadTerminalDrawer from './LazyThreadTerminalDrawer.svelte';
  import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let surface = $derived<ThreadTerminalSurfaceContext>({
    paneId: pane.paneId,
    get threadId() { return pane.threadId; },
    get workspacePath() { return pane.thread?.workspacePath; },
    setVisible(value) {
      pane.setShowTerminal(value);
    },
    acquireResizeLease() {
      return pane.scrollController?.pauseAutoScroll() ?? null;
    },
    consumeFocusRequest() {
      return pane.consumeTerminalFocusRequest();
    },
  });
</script>

{#if pane.showTerminal && pane.threadId}
  {#key pane.threadId}
    <LazyThreadTerminalDrawer {surface} />
  {/key}
{/if}
