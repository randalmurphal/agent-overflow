<script lang="ts">
  // The thread title's regenerate affordance: a refresh glyph beside the
  // title that re-titles the thread from its conversation so far. Sits in
  // ChatHeader only — the terminal pane header shares the title handle but a
  // terminal has no conversation to title from. Under compact the same
  // action is a row of the header's actions menu instead (ChatHeaderActions).
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import type { PaneSession } from '../../stores/threadPaneRoles';
  import { createThreadTitleRegenerate } from './threadTitleRegenerate.svelte';

  let { pane }: { pane: PaneSession } = $props();
  const regenerate = createThreadTitleRegenerate(() => pane);
</script>

{#if pane.thread}
  <PaneHeaderIconButton
    label="Regenerate title"
    title={regenerate.title}
    disabled={regenerate.pending || regenerate.ungranted}
    testId="thread-title-regenerate"
    pending={regenerate.pending}
    onclick={() => regenerate.run()}
  >
    <Icon icon={RefreshCw} size={12} strokeWidth={2} class={regenerate.pending ? 'animate-spin' : ''} />
  </PaneHeaderIconButton>
{/if}
