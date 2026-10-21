<script lang="ts">
  // The header strip every tool pane wears: the status line, a Back control,
  // the tool's title as the pane drag handle, the tool's own segments, and
  // the close control. A tool body renders it as its first row so the body
  // can fill the segments (a breadcrumb, a filter summary); ToolPane renders
  // it bare while the body chunk loads.
  //
  // Back has one meaning at a time. With a detail step open the tool passes
  // `onBack` and the control steps back to its list; otherwise the compact
  // layout's control returns to the thread list, as the chat header's does.
  import type { Snippet } from 'svelte';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import Icon from '../primitives/Icon.svelte';
  import PaneHeaderLine from './PaneHeaderLine.svelte';
  import PaneHeaderIconButton from './PaneHeaderIconButton.svelte';
  import PaneCloseButton from './PaneCloseButton.svelte';
  import { isCompactLayout, showCompactList } from '../../stores/layoutMode.svelte';
  import type { ToolPaneKind } from '../../stores/paneLayout.svelte';
  import { toolPaneMeta } from './toolPaneMeta';

  interface Props {
    paneId: string;
    kind: ToolPaneKind;
    onPaneDragStart?: (event: DragEvent) => void;
    /** Step back out of the tool's detail. Omit at the tool's list. */
    onBack?: () => void;
    backLabel?: string;
    backTestId?: string;
    children?: Snippet;
  }

  let {
    paneId,
    kind,
    onPaneDragStart,
    onBack,
    backLabel = 'Back',
    backTestId = 'tool-pane-back',
    children,
  }: Props = $props();

  let compact = $derived(isCompactLayout());
  let title = $derived(toolPaneMeta(kind).title);
</script>

<div
  data-testid="tool-pane-header"
  class="relative z-10 flex min-w-0 shrink-0 items-center gap-1.5 border-b border-border-subtle bg-transparent px-3 py-2 compact:pt-[max(0.5rem,env(safe-area-inset-top))]"
>
  <PaneHeaderLine {paneId} />
  {#if onBack}
    <PaneHeaderIconButton label={backLabel} testId={backTestId} onclick={onBack}>
      <Icon icon={ChevronLeft} size={14} strokeWidth={2} />
    </PaneHeaderIconButton>
  {:else if compact}
    <PaneHeaderIconButton label="Back to threads" testId="compact-back" onclick={showCompactList}>
      <Icon icon={ArrowLeft} size={14} strokeWidth={2} />
    </PaneHeaderIconButton>
  {/if}
  <!-- The title is the pane drag handle, as a thread pane's is. A tool has
       no name of its own to edit, so there is no rename. -->
  <button
    type="button"
    draggable={onPaneDragStart != null && !compact}
    ondragstart={(event) => onPaneDragStart?.(event)}
    data-testid="tool-pane-title"
    class={[
      'shrink-0 truncate rounded-[var(--radius-field)] border-none bg-transparent px-1.5 py-0.5 text-left text-sm font-medium text-fg transition-colors hover:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:select-none compact:py-1.5 compact:text-base',
      onPaneDragStart && !compact ? 'cursor-grab active:cursor-grabbing' : 'cursor-default',
    ].join(' ')}
  >{title}</button>
  <div class="flex min-w-0 flex-1 items-center">
    {@render children?.()}
  </div>
  {#if !compact}
    <PaneCloseButton {paneId} />
  {/if}
</div>
