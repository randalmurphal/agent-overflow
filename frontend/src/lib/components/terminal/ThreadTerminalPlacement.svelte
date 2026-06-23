<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { leaseDuringSettle } from '../../utils/scrollLeaseDuringTransition';
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
    settleAfterAsyncMount() {
      // Cold first open: the lazy drawer commits after setShowTerminal's
      // 2-rAF open lease has released, and the controller has no scrollEl RO.
      // Re-run the same settle lease so a stuck-to-bottom timeline re-pins to
      // the new (shorter) bottom instead of leaving the latest messages hidden
      // behind the drawer. No-op when no controller is registered.
      leaseDuringSettle(pane.scrollController);
    },
  });
</script>

{#if pane.showTerminal && pane.terminalThreadId}
  {#key pane.terminalThreadId}
    <LazyThreadTerminalDrawer {surface} />
  {/key}
{/if}
