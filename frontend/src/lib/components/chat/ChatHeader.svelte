<script lang="ts">
  // Chat header: the shared pane title (PaneTitleHandle — renameable,
  // draggable, focus-outlined) plus an attention dot on the left, and the
  // action cluster on the right (ChatHeaderActions: PR/diff badges, Open,
  // git actions, terminal). Nothing here needs to render mode chrome.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { resolvePaneAttentionDot } from '../panes/paneAttention';
  import PaneTitleHandle from '../panes/PaneTitleHandle.svelte';
  import PaneCloseButton from '../panes/PaneCloseButton.svelte';
  import ThreadTitleRegenerateButton from './ThreadTitleRegenerateButton.svelte';
  import ChatHeaderActions from './ChatHeaderActions.svelte';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { pane, onPaneDragStart }: Props = $props();
  let attentionDot = $derived(resolvePaneAttentionDot(pane.thread ?? null));
  let titleGlow = $derived(attentionDot?.pill.glowClass ?? '');
</script>

{#if pane.thread}
  <div
    data-testid="chat-header"
    class="flex items-center gap-2 border-b border-border-subtle bg-transparent px-5 py-2 shrink-0 min-w-0 flex-nowrap"
  >
    {#if attentionDot}
      <span
        aria-label={attentionDot.pill.label}
        title={attentionDot.pill.label}
        class={[
          'shrink-0 h-2.5 w-2.5 rounded-full',
          attentionDot.pill.dotClass,
          attentionDot.pill.pulse ? 'animate-pulse' : '',
          attentionDot.pill.glowClass ?? '',
        ].join(' ')}
        data-testid="pane-attention-dot"
        data-pane-id={pane.paneId}
        data-status={attentionDot.status}
      ></span>
    {/if}
    <PaneTitleHandle
      {pane}
      {onPaneDragStart}
      glowClass={titleGlow}
      titleTestId="chat-header-title"
      inputTestId="chat-header-title-input"
    />
    <ThreadTitleRegenerateButton {pane} />
    <PaneCloseButton paneId={pane.paneId} testId="pane-close" />

    <ChatHeaderActions {pane} />
  </div>
{/if}
