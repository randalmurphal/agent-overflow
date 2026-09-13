<script lang="ts">
  // Chat header: the project crumb and the shared pane title
  // (PaneTitleHandle — renameable, draggable), the action cluster on the
  // right (ChatHeaderActions: PR/diff badges, Open, git actions, terminal),
  // and the status line over the bottom border (PaneHeaderLine: focus, or a
  // background pane's attention state). Nothing here needs to render mode
  // chrome.
  //
  // Desktop is one row: `project / title`, then the regenerate glyph, the
  // pane close control and the cluster. Compact is two: the way back to
  // the list, the title (one line that fades and swipes rather than
  // wrapping), the diff badge and the actions menu; then a full-width
  // facts line — machine, project, branch, worktree — which is where the
  // phone shows and changes the workspace, since the composer's strip does
  // not mount there. The pane close control goes under compact: closing
  // the only pane would leave the thread screen empty with no list under
  // it. Title regeneration moves into the actions menu.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import PaneTitleHandle from '../panes/PaneTitleHandle.svelte';
  import PaneHeaderLine from '../panes/PaneHeaderLine.svelte';
  import PaneCloseButton from '../panes/PaneCloseButton.svelte';
  import ThreadTitleRegenerateButton from './ThreadTitleRegenerateButton.svelte';
  import ChatHeaderActions from './ChatHeaderActions.svelte';
  import ChatHeaderProject from './ChatHeaderProject.svelte';
  import ChatHeaderFactsLine from './ChatHeaderFactsLine.svelte';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import { isCompactLayout, showCompactList } from '../../stores/layoutMode.svelte';
  import { headerSegmentSeparatorClasses } from './headerSegmentClasses';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { pane, onPaneDragStart }: Props = $props();
  let compact = $derived(isCompactLayout());
</script>

{#if pane.thread}
  <!-- The timeline fade overdraws its composited clip by one pixel. Keep the
       header's existing bottom border (and the status line over it) above
       that overlap. -->
  <div
    data-testid="chat-header"
    class="relative z-10 flex items-center gap-2 border-b border-border-subtle bg-transparent px-5 py-2 shrink-0 min-w-0 flex-nowrap compact:flex-wrap compact:gap-y-1 compact:px-3 compact:pt-[max(0.5rem,env(safe-area-inset-top))]"
  >
    <PaneHeaderLine paneId={pane.paneId} thread={pane.thread} />
    {#if compact}
      <div class="flex w-full min-w-0 items-center gap-2" data-testid="chat-header-title-row">
        <PaneHeaderIconButton label="Back to threads" testId="compact-back" onclick={showCompactList}>
          <Icon icon={ArrowLeft} size={14} strokeWidth={2} />
        </PaneHeaderIconButton>
        <PaneTitleHandle
          {pane}
          fade
          titleTestId="chat-header-title"
          inputTestId="chat-header-title-input"
        />
        <ChatHeaderActions {pane} />
      </div>
      <ChatHeaderFactsLine {pane} />
    {:else}
      <ChatHeaderProject {pane} class="shrink-0 max-w-[14rem] text-text-secondary" />
      {#if pane.thread.projectId}
        <span class={headerSegmentSeparatorClasses} aria-hidden="true">/</span>
      {/if}
      <PaneTitleHandle
        {pane}
        {onPaneDragStart}
        titleTestId="chat-header-title"
        inputTestId="chat-header-title-input"
      />
      <ThreadTitleRegenerateButton {pane} />
      <PaneCloseButton paneId={pane.paneId} testId="pane-close" />
      <ChatHeaderActions {pane} />
    {/if}
  </div>
{/if}
