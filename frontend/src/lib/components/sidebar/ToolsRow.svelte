<script lang="ts">
  // The sidebar's tools row (docs/specs/pull-requests.md, "Tools row"): one
  // button per tool pane kind, in the fixed order of `TOOL_PANES`. The active
  // tool (the focused tool pane, else the tool used last) shows its icon and
  // label and takes the row's spare width; the others show their icon, with
  // the name as hover text.
  //
  // A click opens the tool in the focused pane like a thread; ctrl/cmd+click
  // opens it in a new pane beside the focused one. Off compact a button drags
  // like a thread row: the pane strip previews the slot and the drop places
  // the tool there (`openTool` with `insertIndex`), moving it when open.
  //
  // Only Workflows carries a count: the durable attention count (UI-SPEC §7),
  // hydrated at boot and live off `workflow:item-state`, so a missed OS
  // notification never loses work. It hides at zero.

  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import CountBadge from '../primitives/CountBadge.svelte';
  import { TOOL_PANES } from '../panes/toolPaneMeta';
  import { getWorkflowAttentionCount } from '../../stores/workflowRuns.svelte';
  import { activeToolKind, isToolOpen, openTool } from '../../stores/toolPanes.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import type { ToolPaneKind } from '../../stores/paneLayout.svelte';
  import { isModClick } from '../../utils/modClick';
  import {
    beginToolPaneDrag,
    encodeToolDragPayload,
    endToolPaneDrag,
    TOOL_PANE_DRAG_MIME,
  } from '../../utils/threadDragPayload';

  let active = $derived(activeToolKind());
  let compact = $derived(isCompactLayout());
  let workflowsCount = $derived(getWorkflowAttentionCount());

  function hoverText(kind: ToolPaneKind, title: string): string {
    if (kind !== 'workflows' || workflowsCount <= 0) return title;
    return `${title} · ${workflowsCount} ${workflowsCount === 1 ? 'needs' : 'need'} you`;
  }

  function handleDragStart(event: DragEvent, kind: ToolPaneKind): void {
    if (compact || !event.dataTransfer) {
      event.preventDefault();
      return;
    }
    event.dataTransfer.effectAllowed = 'move';
    event.dataTransfer.setData(TOOL_PANE_DRAG_MIME, encodeToolDragPayload({ kind }));
    // The pane strip reads this during dragover, where the DataTransfer
    // hands back nothing.
    beginToolPaneDrag({ kind });
  }
</script>

<div class="flex shrink-0 items-center gap-1 border-t border-border-subtle p-2" data-testid="sidebar-tools-row">
  {#each TOOL_PANES as tool (tool.kind)}
    {@const isActive = tool.kind === active}
    {@const title = hoverText(tool.kind, tool.title)}
    <Button
      variant="ghost"
      size="sm"
      onclick={(event) => openTool(tool.kind, { newPane: isModClick(event) })}
      draggable={!compact}
      ondragstart={(event) => handleDragStart(event, tool.kind)}
      ondragend={endToolPaneDrag}
      pressed={isToolOpen(tool.kind)}
      testId={`sidebar-tool-${tool.kind}`}
      {title}
      ariaLabel={isActive ? undefined : title}
      class={isActive ? 'min-w-0 flex-1 justify-start' : 'shrink-0 justify-center px-2'}
    >
      {#snippet leading()}
        <Icon icon={tool.icon} size={13} strokeWidth={2} class="shrink-0 opacity-80" />
      {/snippet}
      {#snippet children()}
        {#if isActive}
          <span class="min-w-0 flex-1 truncate text-left" data-testid="sidebar-tool-label">{tool.title}</span>
        {/if}
        {#if tool.kind === 'workflows'}
          <CountBadge count={workflowsCount} tone="warning" testId="sidebar-tool-workflows-badge" />
        {/if}
      {/snippet}
    </Button>
  {/each}
</div>
