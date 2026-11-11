<script lang="ts">
  // The sidebar's tools row (docs/specs/pull-requests.md, "Tools row"): one
  // control per tool pane kind, in the fixed order of `TOOL_PANES`. The
  // active tool (the focused tool pane, else the tool used last) shows its
  // icon and label and takes the row's spare width; the others show their
  // icon, with the name as hover text.
  //
  // A click, Enter or Space opens the tool in the focused pane like a
  // thread; with ctrl/cmd it opens in a new pane beside the focused one.
  // Off compact a control drags like a thread row: the pane strip previews
  // the slot and the drop places the tool there (`openTool` with
  // `insertIndex`), moving it when open. Each control is a `role="button"`
  // element styled by `buttonClasses`, not a native button, because Firefox
  // starts no drag from a `<button>`.
  //
  // Only Workflows carries a count: the durable attention count (UI-SPEC §7),
  // hydrated at boot and live off `workflow:item-state`, so a missed OS
  // notification never loses work. It hides at zero.

  import Icon from '../primitives/Icon.svelte';
  import CountBadge from '../primitives/CountBadge.svelte';
  import { buttonClasses } from '../primitives/buttonClasses';
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

  function handleKeydown(event: KeyboardEvent, kind: ToolPaneKind): void {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    openTool(kind, { newPane: isModClick(event) });
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
    {@const pressed = isToolOpen(tool.kind)}
    {@const title = hoverText(tool.kind, tool.title)}
    <div
      role="button"
      tabindex="0"
      aria-pressed={pressed ? 'true' : 'false'}
      aria-label={isActive ? undefined : title}
      {title}
      draggable={compact ? undefined : 'true'}
      data-testid={`sidebar-tool-${tool.kind}`}
      class={buttonClasses({
        variant: 'ghost',
        size: 'sm',
        pressed,
        class: `compact:select-none ${isActive ? 'min-w-0 flex-1 justify-start' : 'shrink-0 justify-center px-2'}`,
      })}
      onclick={(event) => openTool(tool.kind, { newPane: isModClick(event) })}
      onkeydown={(event) => handleKeydown(event, tool.kind)}
      ondragstart={(event) => handleDragStart(event, tool.kind)}
      ondragend={endToolPaneDrag}
    >
      <Icon icon={tool.icon} size={13} strokeWidth={2} class="shrink-0 opacity-80" />
      {#if isActive}
        <span class="min-w-0 flex-1 truncate text-left" data-testid="sidebar-tool-label">{tool.title}</span>
      {/if}
      {#if tool.kind === 'workflows'}
        <CountBadge count={workflowsCount} tone="warning" testId="sidebar-tool-workflows-badge" />
      {/if}
    </div>
  {/each}
</div>
