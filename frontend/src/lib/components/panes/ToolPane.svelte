<script lang="ts">
  // The body of a tool pane (toolPanes.svelte.ts): an app-wide surface in the
  // strip with no thread behind it. PaneHost owns the section around it
  // (measuring, width, focus on pointerdown, reorder drops); this component
  // owns the render boundary and the lazily loaded tool body, which renders
  // its own ToolPaneHeader.
  import type { Component } from 'svelte';
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import ToolPaneHeader from './ToolPaneHeader.svelte';
  import { getFocusedPaneId } from '../../stores/panes.svelte';
  import { noteToolUsed } from '../../stores/toolPanes.svelte';
  import type { ToolPaneKind } from '../../stores/paneLayout.svelte';

  interface Props {
    paneId: string;
    kind: ToolPaneKind;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  type ToolLoader = () => Promise<{ default: Component<any> }>;

  const TOOL_LOADERS = {
    pulls: () => import('../pulls/PullsPane.svelte'),
    workflows: () => import('../workflows/WorkflowsPane.svelte'),
  } satisfies Record<ToolPaneKind, ToolLoader>;

  let { paneId, kind, onPaneDragStart }: Props = $props();

  // Captured ONCE: a tool pane's kind is fixed for its lifetime, and a divider
  // drag replaces the layout item behind these props every frame. A loader
  // called in the template would mint a promise per frame and remount the
  // body mid-drag (the CompanionPane rule).
  // svelte-ignore state_referenced_locally
  const bodyLoad = TOOL_LOADERS[kind]();

  // Focusing a tool pane is using it: the sidebar's tools row labels the tool
  // used last once focus moves elsewhere.
  let focused = $derived(getFocusedPaneId() === paneId);
  $effect(() => {
    if (focused) noteToolUsed(kind);
  });
</script>

<RenderBoundary label="This tool pane" testId="pane-render-error">
  <div class="flex h-full min-h-0 flex-col" data-testid={`tool-pane-${kind}`}>
    {#await bodyLoad}
      <ToolPaneHeader {paneId} {kind} {onPaneDragStart} />
      <div class="flex flex-1 items-center justify-center text-xs text-fg-muted">Loading…</div>
    {:then { default: ToolBody }}
      <ToolBody {paneId} {onPaneDragStart} />
    {:catch err}
      <ToolPaneHeader {paneId} {kind} {onPaneDragStart} />
      <div class="flex flex-1 items-center justify-center px-4 text-sm text-error" data-testid="tool-pane-load-error">
        Failed to load: {err instanceof Error ? err.message : String(err)}
      </div>
    {/await}
  </div>
</RenderBoundary>
