<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import Icon from '../primitives/Icon.svelte';
  import SquareTerminal from 'lucide-svelte/icons/square-terminal';
  import PaneTitleHandle from '../panes/PaneTitleHandle.svelte';
  import PaneCloseButton from '../panes/PaneCloseButton.svelte';
  import TerminalSurface from './TerminalSurface.svelte';
  import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { pane, onPaneDragStart }: Props = $props();

  // Project name shown after the title. A home terminal (no project) shows
  // "~" to signal it's rooted at the home dir; a per-project terminal shows
  // the project name — empty until the projects store has the row, never a
  // misleading "~".
  let projectLabel = $derived.by(() => {
    const projectId = pane.thread?.projectId;
    if (!projectId) return '~';
    return getProject(projectId)?.project.name ?? '';
  });

  // Pane-flavored surface. Unlike the bottom drawer, a full pane has no
  // visibility to toggle (setVisible is a no-op — closing the last tab leaves
  // the empty state, not a collapse) and no auto-scrolling timeline to pause
  // for a resize lease. Focus intent still delegates to the pane so the
  // new-pane create helper can latch focus into the fresh shell.
  let surface = $derived<ThreadTerminalSurfaceContext>({
    paneId: pane.paneId,
    get threadId() { return pane.threadId; },
    get workspacePath() { return pane.thread?.workspacePath; },
    setVisible() {},
    acquireResizeLease() { return null; },
    consumeFocusRequest() { return pane.consumeTerminalFocusRequest(); },
  });
</script>

<div
  class="flex h-full min-h-0 flex-col overflow-hidden"
  data-ui-surface="terminal"
  data-thread-id={pane.thread?.id}
>
  <!-- Header mirrors a chat pane's: a leading glyph, then the shared
       PaneTitleHandle (renameable / draggable / focus-outlined), the project
       name, and the close-pane X grouped right beside it (not far-right). -->
  <header
    class="flex items-center gap-2 h-8 shrink-0 px-2 bg-surface-1 border-b border-border text-xs min-w-0"
  >
    <Icon
      icon={SquareTerminal}
      size={12}
      strokeWidth={2}
      class="opacity-90 shrink-0 text-fg-muted"
    />
    <PaneTitleHandle
      {pane}
      {onPaneDragStart}
      titleTestId="terminal-pane-title"
      inputTestId="terminal-pane-title-input"
    />
    {#if projectLabel}
      <span
        class="truncate min-w-0 text-fg-muted"
        title={pane.thread?.workspacePath ?? projectLabel}
        data-testid="terminal-pane-project"
      >
        {projectLabel}
      </span>
    {/if}
    <!-- A focused terminal pane has no keyboard close: pane.close (mod+w) is
         gated when:'!terminalFocus' so ctrl-w reaches the shell. This button
         is the affordance that closes the pane. The PTY keeps running (close
         removes the pane, never CloseTerminal); reopening the thread reattaches. -->
    <PaneCloseButton paneId={pane.paneId} testId="terminal-pane-close" />
  </header>

  <!-- Keyed on the thread so swapping the pane's terminal thread remounts the
       surface — handle is fetched once at init, so without the remount it would
       stay pinned to the old thread (mirrors ThreadTerminalPlacement's key). -->
  {#key pane.threadId}
    <TerminalSurface {surface} collapsible={false} />
  {/key}
</div>
