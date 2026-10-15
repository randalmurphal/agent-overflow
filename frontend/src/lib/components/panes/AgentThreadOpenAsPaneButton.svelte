<script lang="ts">
  // Open as Pane, in an agent thread companion's header: the thread moves to
  // a normal thread pane in the same slot, which keeps it after the source
  // pane leaves its thread. Renders only in that companion. A failure is
  // reported by the store as a toast.
  import Columns2 from '@lucide/svelte/icons/columns-2';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderIconButton from './PaneHeaderIconButton.svelte';
  import { openAgentThreadCompanionAsPane, AGENT_THREAD_COMPANION_KIND } from '../../stores/agentThreadCompanion';
  import { getCompanionPane } from '../../stores/companionPanes.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let opening = $state(false);
  let isAgentThreadCompanion = $derived(getCompanionPane(pane.paneId)?.kind === AGENT_THREAD_COMPANION_KIND);

  async function open(): Promise<void> {
    if (opening) return;
    opening = true;
    try {
      await openAgentThreadCompanionAsPane(pane.paneId);
    } finally {
      opening = false;
    }
  }
</script>

{#if isAgentThreadCompanion}
  <PaneHeaderIconButton
    label="Open as Pane"
    testId="agent-thread-open-as-pane"
    disabled={opening}
    onclick={open}
  >
    <Icon icon={Columns2} size={12} strokeWidth={2} />
  </PaneHeaderIconButton>
{/if}
