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
    get threadId() { return pane.terminalThreadId; },
    get workspacePath() { return pane.thread?.workspacePath; },
    setVisible(value) {
      pane.setShowTerminal(value);
    },
    canAdoptOpenedTerminal(threadID, workspacePath) {
      return pane.canAdoptOpenedTerminal(threadID, workspacePath);
    },
    acquireResizeLease() {
      return pane.scrollController?.pauseAutoScroll() ?? null;
    },
    consumeFocusRequest() {
      return pane.consumeTerminalFocusRequest();
    },
  });
</script>

{#if pane.showTerminal && pane.terminalThreadId}
  {#key pane.terminalThreadId}
    <LazyThreadTerminalDrawer {surface} />
  {/key}
{/if}
