<script lang="ts">
  // Chat header: the shared pane title (PaneTitleHandle — renameable,
  // draggable), the action cluster on the right (ChatHeaderActions: PR/diff
  // badges, Open, git actions, terminal), and the status line over the
  // bottom border (PaneHeaderLine: focus, or a background pane's attention
  // state). Nothing here needs to render mode chrome.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import PaneTitleHandle from '../panes/PaneTitleHandle.svelte';
  import PaneHeaderLine from '../panes/PaneHeaderLine.svelte';
  import PaneCloseButton from '../panes/PaneCloseButton.svelte';
  import ThreadTitleRegenerateButton from './ThreadTitleRegenerateButton.svelte';
  import ChatHeaderActions from './ChatHeaderActions.svelte';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import { isCompactLayout, showCompactList } from '../../stores/layoutMode.svelte';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { pane, onPaneDragStart }: Props = $props();
  // Compact: the way back to the list leads the header, and the pane
  // close control goes, because the only pane closing would leave the
  // thread screen empty with no list under it.
  let compact = $derived(isCompactLayout());
</script>

{#if pane.thread}
  <!-- The timeline fade overdraws its composited clip by one pixel. Keep the
       header's existing bottom border (and the status line over it) above
       that overlap. -->
  <div
    data-testid="chat-header"
    class="relative z-10 flex items-center gap-2 border-b border-border-subtle bg-transparent px-5 py-2 shrink-0 min-w-0 flex-nowrap compact:flex-wrap compact:gap-y-1 compact:px-3"
  >
    <PaneHeaderLine paneId={pane.paneId} thread={pane.thread} />
    <div class="contents compact:flex min-w-0 items-center gap-2 compact:w-full compact:items-start" data-testid="chat-header-title-row">
    {#if compact}
      <PaneHeaderIconButton label="Back to threads" testId="compact-back" onclick={showCompactList}>
        <Icon icon={ArrowLeft} size={14} strokeWidth={2} />
      </PaneHeaderIconButton>
    {/if}
    <PaneTitleHandle
      {pane}
      {onPaneDragStart}
      wrap={compact}
      titleTestId="chat-header-title"
      inputTestId="chat-header-title-input"
    />
    </div>
    <div class="contents compact:flex min-w-0 items-center gap-2 ml-auto compact:w-full" data-testid="chat-header-actions-row">
    <ThreadTitleRegenerateButton {pane} />
    {#if !compact}
      <PaneCloseButton paneId={pane.paneId} testId="pane-close" />
    {/if}

    <ChatHeaderActions {pane} />
    </div>
  </div>
{/if}
