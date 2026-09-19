<script lang="ts">
  // Keep, in a side chat's pane header: the scratch thread takes back the
  // mode recorded at the fork, which puts it in the sidebar, and the
  // companion is replaced by an ordinary thread pane in the same slot.
  //
  // Renders only on a scratch thread, which is the only thread there is
  // anything to keep about. A promotion failure is reported by the store as
  // a toast; the pane stays exactly as it was.
  import Save from '@lucide/svelte/icons/save';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderIconButton from './PaneHeaderIconButton.svelte';
  import { keepSideChat } from '../../stores/sideChat';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { isScratchThreadMode } from '../../utils/threadModes';

  let { pane }: { pane: ThreadPane } = $props();

  let keeping = $state(false);
  let isSideChat = $derived(isScratchThreadMode(pane.thread?.mode));

  async function keep(): Promise<void> {
    if (keeping) return;
    keeping = true;
    try {
      await keepSideChat(pane.paneId);
    } finally {
      keeping = false;
    }
  }
</script>

{#if isSideChat}
  <PaneHeaderIconButton
    label="Keep this side chat"
    testId="side-chat-keep"
    disabled={keeping}
    onclick={keep}
  >
    <Icon icon={Save} size={12} strokeWidth={2} />
  </PaneHeaderIconButton>
{/if}
