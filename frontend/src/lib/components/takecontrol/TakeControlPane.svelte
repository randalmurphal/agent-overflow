<script lang="ts">
  // The take-control pane: a terminal mirror of the claude-tui session hosted
  // by its paired source thread pane. The mirrored thread is PINNED at open —
  // like every companion, a source-pane thread switch closes this pane
  // (ThreadPane.commitIncomingThread → closeCompanionsForSource) instead of
  // re-attaching it, because a terminal that silently swaps sessions risks
  // keystrokes landing in the wrong one. The effects below are the defensive
  // layer behind that store seam: close if the source pane disappears, stops
  // showing the pinned thread, or the provider session dies. The xterm
  // surface + control-lease toggle are layered in by TakeControlTerminal.
  import X from 'lucide-svelte/icons/x';
  import { closeCompanion, getCompanionPane } from '../../stores/companionPanes.svelte';
  import { getPane } from '../../stores/panes.svelte';
  import { wailsEventOn } from '../../stores/wailsEvents';
  import type { SessionDiedEvent } from '../../types/events';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import TakeControlTerminal from './TakeControlTerminal.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';

  let { paneId }: { paneId: string } = $props();

  // Captured at init: the pairing and the mirrored thread are fixed for this
  // pane's lifetime. Any change that would invalidate them closes the pane
  // rather than retargeting it.
  // svelte-ignore state_referenced_locally
  const sourcePaneId = getCompanionPane(paneId)?.sourcePaneId ?? null;
  const targetThreadId = (() => {
    const sourcePane = sourcePaneId ? getPane(sourcePaneId) : null;
    return sourcePane?.thread?.provider === 'claude-tui' ? sourcePane.threadId : null;
  })();

  // Close exactly once. Deferred to a microtask so we never mutate pane/layout
  // state synchronously from inside a reactive read (which would re-enter the
  // render flush); the deferral also coalesces the several close triggers below
  // into a single teardown.
  let closing = false;
  function requestClose(): void {
    if (closing) return;
    closing = true;
    queueMicrotask(() => closeCompanion(paneId));
  }

  // Nothing live to mirror → close (no dangling pane on the side). The
  // threadId comparison is defense in depth: the store seam closes this pane
  // before the source pane commits a different thread, so a mismatch here
  // means some new path bypassed it.
  $effect(() => {
    const sourcePane = sourcePaneId ? getPane(sourcePaneId) : null;
    if (!sourcePane || !targetThreadId || sourcePane.threadId !== targetThreadId) {
      requestClose();
    }
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
      <TakeControlTerminal {paneId} threadId={targetThreadId} />
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
