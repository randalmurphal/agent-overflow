<script lang="ts">
  // The take-control pane: a terminal mirror of the claude-tui session hosted by
  // its paired source thread pane. This component owns the pairing lifecycle —
  // it follows the source pane's current claude-tui thread, and closes itself
  // the moment there is nothing live to mirror (source pane gone, source
  // switched to a non-claude-tui thread, or the provider session died). The
  // xterm surface + control-lease toggle are layered in by TakeControlTerminal.
  import X from 'lucide-svelte/icons/x';
  import {
    closeTakeControlForLostSource,
    sourcePaneForTakeControl,
  } from '../../stores/takeControl.svelte';
  import { wailsEventOn } from '../../stores/wailsEvents';
  import type { SessionDiedEvent } from '../../types/events';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import TakeControlTerminal from './TakeControlTerminal.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';

  let { paneId }: { paneId: string } = $props();

  // Reactively resolve the paired source pane and the claude-tui thread it
  // currently hosts. This IS the "switch follows" contract: the terminal always
  // mirrors whatever claude-tui session the source pane is showing right now.
  let sourcePane = $derived(sourcePaneForTakeControl(paneId));
  let sourceThread = $derived(sourcePane?.thread ?? null);
  let targetThreadId = $derived(
    sourceThread?.provider === 'claude-tui' ? sourcePane?.threadId ?? null : null,
  );

  // Close exactly once. Deferred to a microtask so we never mutate pane/layout
  // state synchronously from inside a reactive read (which would re-enter the
  // render flush); the deferral also coalesces the several close triggers below
  // into a single teardown.
  let closing = false;
  function requestClose(): void {
    if (closing) return;
    closing = true;
    queueMicrotask(() => closeTakeControlForLostSource(paneId));
  }

  // Nothing live to mirror → close (no dangling pane on the side).
  $effect(() => {
    if (!sourcePane || !targetThreadId) requestClose();
  });

  // The provider session ending closes the pane too.
  $effect(() => {
    const threadId = targetThreadId;
    if (!threadId) return;
    return wailsEventOn<SessionDiedEvent>('provider:session_died', (evt) => {
      if (evt.threadId === threadId) requestClose();
    });
  });
</script>

<div class="flex h-full min-h-0 flex-col bg-terminal-bg" data-testid={`take-control-${paneId}`}>
  <header
    class="flex items-center gap-2 px-3 py-1.5 border-b border-border-subtle/70 text-xs text-fg-muted shrink-0"
  >
    <ProviderIcon provider="claude-tui" size={13} />
    <span class="font-medium text-fg">Claude TUI</span>
    <span class="opacity-70">terminal</span>
    <div class="ml-auto">
      <IconButton label="Close take-control terminal" size="sm" onClick={requestClose}>
        {#snippet children()}
          <Icon icon={X} size={14} />
        {/snippet}
      </IconButton>
    </div>
  </header>

  <div class="flex-1 min-h-0">
    {#if targetThreadId}
      {#key targetThreadId}
        <TakeControlTerminal {paneId} threadId={targetThreadId} />
      {/key}
    {:else}
      <div class="flex h-full items-center justify-center text-sm text-fg-muted">
        <span class="inline-flex items-center gap-2">
          <Icon icon={X} size={14} class="opacity-60" />
          No live Claude TUI session.
        </span>
      </div>
    {/if}
  </div>
</div>
