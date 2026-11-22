<script lang="ts">
  // Chat header: the project crumb and the shared pane title
  // (PaneTitleHandle — renameable, draggable), the action cluster on the
  // right (ChatHeaderActions: PR/diff badges, git actions, the Thread
  // actions menu), and the status line over the bottom border
  // (PaneHeaderLine: focus, or a background pane's attention state). Nothing
  // here needs to render mode chrome.
  //
  // Desktop is one row: `project / title`, then the pane close control and
  // the cluster. The crumb shows its whole name or is not there: the title
  // is what truncates as the header narrows, down to a floor of 8rem or its
  // own width, and below a 36rem header (a pane beside a companion on a
  // laptop screen) the crumb goes. The row is a wrapping flex line whose
  // items are placed by their floors, so the cluster drops to a second row
  // only when the title has nothing left to give, and nothing ever runs
  // past the pane. Compact is two: the way back to the list, the title
  // (one line that fades and swipes rather than wrapping), the diff badge
  // and the actions menu; then a full-width facts line — machine, project,
  // branch, worktree — which is where the phone shows and changes the
  // workspace, since the composer's strip does not mount there. The pane
  // close control goes under compact: closing the only pane would leave the
  // thread screen empty with no list under it.
  //
  // Title regeneration is a row of the actions menu. While a run the user
  // asked for is in flight, a spinner sits beside the title it will replace.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import PaneTitleHandle from '../panes/PaneTitleHandle.svelte';
  import PaneHeaderLine from '../panes/PaneHeaderLine.svelte';
  import PaneCloseButton from '../panes/PaneCloseButton.svelte';
  import PanePromoteButton from '../panes/PanePromoteButton.svelte';
  import SideChatKeepButton from '../panes/SideChatKeepButton.svelte';
  import AgentThreadOpenAsPaneButton from '../panes/AgentThreadOpenAsPaneButton.svelte';
  import ChatHeaderActions from './ChatHeaderActions.svelte';
  import ChatHeaderProject from './ChatHeaderProject.svelte';
  import ChatHeaderFactsLine from './ChatHeaderFactsLine.svelte';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import Icon from '../primitives/Icon.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import { isCompactLayout, showCompactList } from '../../stores/layoutMode.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { titleGenerationPending } from '../../stores/threadTitleGeneration.svelte';
  import { headerSegmentSeparatorClasses } from './headerSegmentClasses';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { pane, onPaneDragStart }: Props = $props();
  let compact = $derived(isCompactLayout());
  let titlePending = $derived(pane.threadId ? titleGenerationPending(pane.threadId) : false);
</script>

{#snippet titlePendingMark()}
  {#if titlePending}
    <span
      role="status"
      aria-label="Regenerating title"
      title="Regenerating title"
      data-testid="thread-title-regenerating"
      class="flex shrink-0 text-fg-hint"
    >
      <SteppedSpinner size={12} animate={!getSettings().lowPowerMode} />
    </span>
  {/if}
{/snippet}

{#if pane.thread}
  <!-- The timeline fade overdraws its composited clip by one pixel. Keep the
       header's existing bottom border (and the status line over it) above
       that overlap. -->
  <div
    data-testid="chat-header"
    data-transport-tap-hold
    class="@container/chat-header relative z-10 flex items-center gap-2 border-b border-border-subtle bg-transparent px-5 py-2 shrink-0 min-w-0 flex-wrap compact:gap-y-1 compact:px-3 compact:pt-[max(0.5rem,env(safe-area-inset-top))]"
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
        {@render titlePendingMark()}
        <SideChatKeepButton {pane} />
        <AgentThreadOpenAsPaneButton {pane} />
        <ChatHeaderActions {pane} />
      </div>
      <ChatHeaderFactsLine {pane} />
    {:else}
      <!-- The crumb never shrinks: past its 14rem cap a name ellipsizes at
           every width, and a narrow header leaves the crumb out. -->
      <ChatHeaderProject {pane} class="shrink-0 max-w-[14rem] text-text-secondary @max-[36rem]/chat-header:hidden" />
      {#if pane.thread.projectId}
        <span class="{headerSegmentSeparatorClasses} @max-[36rem]/chat-header:hidden" aria-hidden="true">/</span>
      {/if}
      <!-- The title's place on the line is its floor: 8rem, or its own
           width when that is less. It grows from there into the spare
           width and never past its text, so a short title keeps the close
           control beside it. -->
      <PaneTitleHandle
        {pane}
        {onPaneDragStart}
        class="grow basis-[8rem] max-w-max"
        titleTestId="chat-header-title"
        inputTestId="chat-header-title-input"
      />
      {@render titlePendingMark()}
      <SideChatKeepButton {pane} />
      <AgentThreadOpenAsPaneButton {pane} />
      <PanePromoteButton paneId={pane.paneId} />
      <PaneCloseButton paneId={pane.paneId} testId="pane-close" />
      <ChatHeaderActions {pane} />
    {/if}
  </div>
{/if}
