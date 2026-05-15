<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { addTerminalChipToPaneDraft } from '../../stores/composerDraftRegistry.svelte';
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
    sendTerminalChip(chip) {
      addTerminalChipToPaneDraft(pane.paneId, chip);
    },
  });
</script>

{#if pane.showTerminal && pane.threadId}
  {#key pane.threadId}
    <LazyThreadTerminalDrawer {surface} />
  {/key}
{/if}
