<script lang="ts">
  // The sidebar's tools row (docs/specs/pull-requests.md, "Tools row"): one
  // button per tool pane kind, in the fixed order of `TOOL_PANES`. The active
  // tool (the focused tool pane, else the tool used last) shows its icon,
  // label and badge and takes the row's spare width; the others show their
  // icon and badge, with the name and count as hover text.
  //
  // A click opens the tool in the focused pane like a thread; ctrl/cmd+click
  // opens it in a new pane beside the focused one. Each badge is the tool's
  // "needs you" count and hides at zero. The workflows count is the durable
  // attention count (UI-SPEC §7): it hydrates at boot and stays live off
  // `workflow:item-state`, so a missed OS notification never loses work.

  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import CountBadge from '../primitives/CountBadge.svelte';
  import { TOOL_PANES } from '../panes/toolPaneMeta';
  import { getWorkflowAttentionCount } from '../../stores/workflowRuns.svelte';
  import { getPullsNeedsYouCount } from '../../stores/pullsAttention.svelte';
  import { activeToolKind, isToolOpen, openTool } from '../../stores/toolPanes.svelte';
  import type { ToolPaneKind } from '../../stores/paneLayout.svelte';
  import { isModClick } from '../../utils/modClick';

  const TONE: Record<ToolPaneKind, 'warning' | 'accent'> = {
    pulls: 'accent',
    workflows: 'warning',
  };

  let active = $derived(activeToolKind());
  let counts = $derived<Record<ToolPaneKind, number>>({
    pulls: getPullsNeedsYouCount(),
    workflows: getWorkflowAttentionCount(),
  });

  function hoverText(title: string, count: number): string {
    if (count <= 0) return title;
    return `${title} · ${count} ${count === 1 ? 'needs' : 'need'} you`;
  }
</script>

<div class="flex shrink-0 items-center gap-1 border-t border-border-subtle p-2" data-testid="sidebar-tools-row">
  {#each TOOL_PANES as tool (tool.kind)}
    {@const isActive = tool.kind === active}
    {@const count = counts[tool.kind]}
    <Button
      variant="ghost"
      size="sm"
      onclick={(event) => openTool(tool.kind, { newPane: isModClick(event) })}
      pressed={isToolOpen(tool.kind)}
      testId={`sidebar-tool-${tool.kind}`}
      title={hoverText(tool.title, count)}
      ariaLabel={isActive ? undefined : hoverText(tool.title, count)}
      class={isActive ? 'min-w-0 flex-1 justify-start' : 'shrink-0 justify-center px-2'}
    >
      {#snippet leading()}
        <Icon icon={tool.icon} size={13} strokeWidth={2} class="shrink-0 opacity-80" />
      {/snippet}
      {#snippet children()}
        {#if isActive}
          <span class="min-w-0 flex-1 truncate text-left" data-testid="sidebar-tool-label">{tool.title}</span>
        {/if}
        <CountBadge {count} tone={TONE[tool.kind]} testId={`sidebar-tool-${tool.kind}-badge`} />
      {/snippet}
    </Button>
  {/each}
</div>
