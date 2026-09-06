<script lang="ts">
  import { threadHasScope } from '../../transport/entityScopes';
  // The thread title's regenerate affordance: a refresh glyph beside the
  // title that re-titles the thread from its conversation so far. Sits in
  // ChatHeader only — the terminal pane header shares the title handle but a
  // terminal has no conversation to title from.
  //
  // Pending state is thread-entity state in threadTitleGeneration (the run
  // outlives this component and its completion arrives as an event), so the
  // spinner survives pane switches and a remount mid-run resumes spinning.
  // RegenerateThreadTitle runs provider CLIs on the host, so it rides
  // `threads:operate`: a remote view-only session gets a disabled button
  // rather than an RPC the transport would refuse.
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import type {
    PaneSession,
  } from '../../stores/threadPaneRoles';
  import {
    regenerateThreadTitle,
    titleGenerationPending,
  } from '../../stores/threadTitleGeneration.svelte';

  let { pane }: { pane: PaneSession } = $props();

  let pending = $derived(pane.threadId ? titleGenerationPending(pane.threadId) : false);
  // Regenerating a title runs a provider turn and writes the thread.
  let ungranted = $derived(!threadHasScope('threads:operate', pane.threadId, pane.thread?.projectId));
</script>

{#if pane.thread}
  <PaneHeaderIconButton
    label="Regenerate title"
    title={ungranted ? 'Not granted to this device' : 'Regenerate title'}
    disabled={pending || ungranted}
    testId="thread-title-regenerate"
    {pending}
    onclick={() => {
      const threadId = pane.threadId;
      if (threadId) void regenerateThreadTitle(threadId);
    }}
  >
    <Icon icon={RefreshCw} size={12} strokeWidth={2} class={pending ? 'animate-spin' : ''} />
  </PaneHeaderIconButton>
{/if}
